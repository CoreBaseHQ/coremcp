package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	mrand "math/rand/v2"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/corebasehq/coremcp/pkg/adapter"
	"github.com/corebasehq/coremcp/pkg/core"
	"github.com/corebasehq/coremcp/pkg/security"
	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

// maxConcurrentCommands caps the number of in-flight command handlers per
// connection. Without a cap, a misbehaving (or malicious) server could push
// commands faster than the agent can process them and exhaust memory.
const maxConcurrentCommands = 32

// MessageType represents the type of WebSocket message
type MessageType string

const (
	// Client -> Server Messages
	MsgTypeAuth      MessageType = "auth"
	MsgTypeHeartbeat MessageType = "heartbeat"
	MsgTypeResponse  MessageType = "response"
	MsgTypeError     MessageType = "error"

	// Server -> Client Messages
	MsgTypeCommand    MessageType = "command"
	MsgTypeConfigSync MessageType = "config_sync"
	MsgTypePing       MessageType = "ping"
)

// CommandType represents the type of command to execute
type CommandType string

const (
	CmdRunSQL      CommandType = "run_sql"
	CmdGetSchema   CommandType = "get_schema"
	CmdListSources CommandType = "list_sources"
	CmdHealthCheck CommandType = "health_check"
)

// WebSocket message structures
type WSMessage struct {
	ID        string          `json:"id"`
	Type      MessageType     `json:"type"`
	Timestamp int64           `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type AuthPayload struct {
	Token    string `json:"token"`
	AgentID  string `json:"agent_id,omitempty"`
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
}

type CommandPayload struct {
	CommandType CommandType            `json:"command_type"`
	Source      string                 `json:"source,omitempty"`
	Query       string                 `json:"query,omitempty"`
	Params      map[string]interface{} `json:"params,omitempty"`
}

type ResponsePayload struct {
	CommandID string      `json:"command_id"`
	Success   bool        `json:"success"`
	Data      interface{} `json:"data,omitempty"`
	Error     string      `json:"error,omitempty"`
}

type ConfigSyncPayload struct {
	Sources         []RemoteSource                   `json:"sources"`
	Security        RemoteSecurity                   `json:"security"`
	RefreshToken    string                           `json:"refresh_token,omitempty"`
	SemanticContext map[string]SemanticSourceContext `json:"semantic_context,omitempty"`
}

// SemanticSourceContext carries business-layer annotations for a single source:
// table/column descriptions, glossary terms, and virtual JOIN hints. Built by
// the Python control plane from user-managed schema annotations and forwarded
// to the agent so it can be returned with get_schema results for LLM prompting.
type SemanticSourceContext struct {
	Tables   map[string]string                 `json:"tables"`
	Columns  map[string]map[string]string      `json:"columns"`
	Glossary map[string]map[string]interface{} `json:"glossary"`
}

type RemoteSource struct {
	Name             string `json:"name"`
	Type             string `json:"type"`
	DSN              string `json:"dsn"`
	ReadOnly         bool   `json:"read_only"`
	NoLock           bool   `json:"no_lock"`
	NormalizeTurkish bool   `json:"normalize_turkish"`
}

type RemoteSecurity struct {
	MaxRowLimit      int      `json:"max_row_limit"`
	EnablePIIMasking bool     `json:"enable_pii_masking"`
	AllowedKeywords  []string `json:"allowed_keywords"`
	BlockedKeywords  []string `json:"blocked_keywords"`
}

// ConnectClient manages WebSocket connection to CoreBase Cloud
type ConnectClient struct {
	serverURL       string
	token           string
	conn            *websocket.Conn
	mu              sync.RWMutex
	writeMu         sync.Mutex    // Protects WebSocket writes
	syncMu          sync.Mutex    // Serialises handleConfigSync invocations
	cmdSem          chan struct{} // Bounds concurrent command handlers
	sources         map[string]core.Source
	semanticContext map[string]SemanticSourceContext
	ctx             context.Context
	cancel          context.CancelFunc
	reconnectDelay  time.Duration
	maxReconnect    int
	agentID         string
	hostname        string
	commandTimeout  time.Duration
	queryValidator  *security.QueryValidator
	queryModifier   *security.QueryModifier
}

var connectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Connect to CoreBase Cloud Platform",
	Long: `Connect to CoreBase Cloud Platform and enable remote management.
	
This command establishes a WebSocket connection to CoreBase Cloud,
allowing you to manage your database connections remotely without
opening any inbound ports. Perfect for secure factory deployments!`,
	RunE: runConnect,
}

func init() {
	connectCmd.Flags().StringP("server", "s", "", "CoreBase Cloud WebSocket URL (e.g., wss://api.corebase.com/ws/agent)")
	connectCmd.Flags().StringP("token", "t", "", "Authentication token (e.g., sk_live_abc123...)")
	connectCmd.Flags().StringP("agent-id", "a", "", "Agent ID (REQUIRED - get from agent creation)")
	connectCmd.Flags().IntP("max-reconnect", "r", 10, "Maximum reconnection attempts (0 for infinite)")
	connectCmd.Flags().DurationP("reconnect-delay", "d", 5*time.Second, "Delay between reconnection attempts")
	connectCmd.Flags().Duration("command-timeout", 30*time.Second, "Timeout for remote commands (SQL/schema)")

	_ = connectCmd.MarkFlagRequired("server")
	_ = connectCmd.MarkFlagRequired("token")
	_ = connectCmd.MarkFlagRequired("agent-id")

	rootCmd.AddCommand(connectCmd)
}

func runConnect(cmd *cobra.Command, args []string) error {
	log.SetOutput(os.Stderr)

	serverURL, _ := cmd.Flags().GetString("server")
	token, _ := cmd.Flags().GetString("token")
	agentID, _ := cmd.Flags().GetString("agent-id")
	maxReconnect, _ := cmd.Flags().GetInt("max-reconnect")
	reconnectDelay, _ := cmd.Flags().GetDuration("reconnect-delay")
	commandTimeout, _ := cmd.Flags().GetDuration("command-timeout")

	// Validate agent ID is provided
	if agentID == "" {
		return fmt.Errorf("agent-id is required - get it from agent creation endpoint")
	}

	// Validate WebSocket URL
	u, err := url.Parse(serverURL)
	if err != nil {
		return fmt.Errorf("invalid server URL: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return fmt.Errorf("invalid server URL: scheme must be ws or wss, got %s", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid server URL: missing host")
	}

	// Get hostname
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	// Create context with cancellation
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	client := &ConnectClient{
		serverURL:       serverURL,
		token:           token,
		cmdSem:          make(chan struct{}, maxConcurrentCommands),
		sources:         make(map[string]core.Source),
		semanticContext: make(map[string]SemanticSourceContext),
		ctx:             ctx,
		cancel:          cancel,
		reconnectDelay:  reconnectDelay,
		maxReconnect:    maxReconnect,
		agentID:         agentID,
		hostname:        hostname,
		commandTimeout:  commandTimeout,
		queryValidator:  security.NewQueryValidator(nil, nil),
		queryModifier:   security.NewQueryModifier(1000),
	}

	fmt.Fprintf(os.Stderr, "[INFO] CoreMCP Connect Mode\n")
	fmt.Fprintf(os.Stderr, "[INFO] Server: %s\n", serverURL)
	fmt.Fprintf(os.Stderr, "[INFO] Agent ID: %s\n", agentID)
	fmt.Fprintf(os.Stderr, "[INFO] Hostname: %s\n", hostname)
	fmt.Fprintf(os.Stderr, "[INFO] Command timeout: %s\n", commandTimeout)
	fmt.Fprintf(os.Stderr, "\n")

	// Start connection with retry logic
	go func() {
		if err := client.connectWithRetry(); err != nil {
			log.Printf("[ERROR] Failed to connect: %v", err)
			cancel()
		}
	}()

	// Wait for shutdown signal
	select {
	case <-sigChan:
		fmt.Fprintf(os.Stderr, "\n[WARN] Received shutdown signal, disconnecting gracefully...\n")
		cancel()
	case <-ctx.Done():
	}

	// Cleanup
	client.cleanup()
	fmt.Fprintf(os.Stderr, "[INFO] CoreMCP Connect stopped.\n")

	return nil
}

func (c *ConnectClient) connectWithRetry() error {
	attempt := 0

	for {
		attempt++

		if c.maxReconnect > 0 && attempt > c.maxReconnect {
			return fmt.Errorf("maximum reconnection attempts (%d) reached", c.maxReconnect)
		}

		if attempt > 1 {
			// Exponential backoff with max delay cap
			baseDelay := c.reconnectDelay
			maxDelay := 5 * time.Minute
			delay := baseDelay
			for i := 1; i < attempt-1; i++ {
				delay *= 2
				if delay > maxDelay {
					delay = maxDelay
					break
				}
			}
			// Add ±10% jitter to avoid thundering-herd reconnects when many
			// agents lose the same upstream simultaneously.
			jitter := time.Duration(float64(delay) * (mrand.Float64()*0.2 - 0.1))
			delay += jitter

			log.Printf("[INFO] Reconnection attempt %d/%d in %v...", attempt, c.maxReconnect, delay)
			select {
			case <-time.After(delay):
			case <-c.ctx.Done():
				return c.ctx.Err()
			}
		}

		if err := c.connect(); err != nil {
			log.Printf("[ERROR] Connection attempt %d failed: %v", attempt, err)
			continue
		}

		// Successfully connected, handle messages
		log.Println("[INFO] Connected to CoreBase Cloud!")

		if err := c.handleMessages(); err != nil {
			log.Printf("[WARN] Connection lost: %v", err)
			c.closeConnection()

			// Don't retry if context is cancelled
			if c.ctx.Err() != nil {
				return c.ctx.Err()
			}

			continue
		}

		return nil
	}
}

func (c *ConnectClient) connect() error {
	// Create WebSocket dialer with timeout
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	// Connect to WebSocket server
	conn, resp, err := dialer.Dial(c.serverURL, nil)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("WebSocket dial failed (HTTP %d): %v", resp.StatusCode, err)
		}
		return fmt.Errorf("WebSocket dial failed: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	// Send authentication message
	authPayload := AuthPayload{
		Token:    c.token,
		AgentID:  c.agentID,
		Version:  Version,
		Hostname: c.hostname,
	}

	if err := c.sendMessage(MsgTypeAuth, authPayload); err != nil {
		c.closeConnection()
		return fmt.Errorf("authentication failed: %w", err)
	}

	return nil
}

func (c *ConnectClient) handleMessages() error {
	connCtx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	// Start heartbeat goroutine tied to this connection's lifetime.
	go c.heartbeatLoop(connCtx)

	// Read messages from server
	for {
		select {
		case <-connCtx.Done():
			return connCtx.Err()
		default:
		}

		c.mu.RLock()
		conn := c.conn
		c.mu.RUnlock()

		if conn == nil {
			return fmt.Errorf("connection is nil")
		}

		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("read message failed: %w", err)
		}

		// Bound concurrent command handlers so a fast-pushing server cannot
		// spawn unbounded goroutines.
		select {
		case c.cmdSem <- struct{}{}:
		case <-connCtx.Done():
			return connCtx.Err()
		}
		msgCopy := msg
		go func() {
			defer func() { <-c.cmdSem }()
			c.handleMessage(&msgCopy)
		}()
	}
}

func (c *ConnectClient) handleMessage(msg *WSMessage) {
	log.Printf("[DEBUG] Received message: type=%s, id=%s", msg.Type, msg.ID)

	switch msg.Type {
	case MsgTypePing:
		// Respond to ping with heartbeat
		if err := c.sendMessage(MsgTypeHeartbeat, map[string]string{
			"status": "alive",
		}); err != nil {
			log.Printf("[WARN] Failed to send heartbeat response: %v", err)
		}

	case MsgTypeCommand:
		c.handleCommand(msg)

	case MsgTypeConfigSync:
		c.handleConfigSync(msg)

	default:
		log.Printf("[WARN] Unknown message type: %s", msg.Type)
	}
}

func (c *ConnectClient) handleCommand(msg *WSMessage) {
	var cmdPayload CommandPayload
	if err := json.Unmarshal(msg.Payload, &cmdPayload); err != nil {
		log.Printf("[ERROR] Failed to parse command payload: %v", err)
		if sendErr := c.sendError(msg.ID, fmt.Sprintf("Invalid command payload: %v", err)); sendErr != nil {
			log.Printf("[WARN] Failed to send error response: %v", sendErr)
		}
		return
	}

	log.Printf("[INFO] Executing command: %s on source: %s", cmdPayload.CommandType, cmdPayload.Source)

	var result interface{}
	var err error

	switch cmdPayload.CommandType {
	case CmdRunSQL:
		if cmdPayload.Source == "" {
			err = fmt.Errorf("source name is required for SQL execution")
		} else if cmdPayload.Query == "" {
			err = fmt.Errorf("query is required for SQL execution")
		} else {
			result, err = c.executeSQL(cmdPayload.Source, cmdPayload.Query, cmdPayload.Params)
		}

	case CmdGetSchema:
		if cmdPayload.Source == "" {
			err = fmt.Errorf("source name is required for schema retrieval")
		} else {
			result, err = c.getSchema(cmdPayload.Source)
		}

	case CmdListSources:
		result = c.listSources()

	case CmdHealthCheck:
		c.mu.RLock()
		sourceCount := len(c.sources)
		c.mu.RUnlock()
		result = map[string]interface{}{
			"status":    "healthy",
			"timestamp": time.Now().Unix(),
			"sources":   sourceCount,
		}

	default:
		err = fmt.Errorf("unknown command type: %s", cmdPayload.CommandType)
	}

	// Send response
	if err != nil {
		log.Printf("[ERROR] Command %s execution failed: %v", cmdPayload.CommandType, err)
		if sendErr := c.sendError(msg.ID, err.Error()); sendErr != nil {
			log.Printf("[ERROR] Failed to send error response for command %s: %v", msg.ID, sendErr)
		} else {
			log.Printf("[INFO] Error response sent successfully for command %s", msg.ID)
		}
	} else {
		log.Printf("[SUCCESS] Command %s executed successfully", cmdPayload.CommandType)
		if sendErr := c.sendResponse(msg.ID, result); sendErr != nil {
			log.Printf("[ERROR] Failed to send success response for command %s: %v", msg.ID, sendErr)
		} else {
			log.Printf("[INFO] Success response sent for command %s (size: %d bytes)", msg.ID, len(fmt.Sprintf("%v", result)))
		}
	}
}

func (c *ConnectClient) handleConfigSync(msg *WSMessage) {
	// Serialise concurrent syncs so two messages can't interleave their
	// teardown/setup phases against c.sources.
	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	var configPayload ConfigSyncPayload
	if err := json.Unmarshal(msg.Payload, &configPayload); err != nil {
		log.Printf("[ERROR] Failed to parse config sync payload: %v", err)
		return
	}

	log.Printf("[INFO] Syncing configuration: %d source(s)", len(configPayload.Sources))

	// Snapshot+detach the old source set, swap in the new security components
	// and semantic context, then release the lock before doing any I/O.
	c.mu.Lock()
	oldSources := c.sources
	c.sources = make(map[string]core.Source)
	c.queryValidator = security.NewQueryValidator(configPayload.Security.AllowedKeywords, configPayload.Security.BlockedKeywords)
	c.queryModifier = security.NewQueryModifier(configPayload.Security.MaxRowLimit)
	if configPayload.SemanticContext != nil {
		c.semanticContext = configPayload.SemanticContext
	} else {
		c.semanticContext = make(map[string]SemanticSourceContext)
	}
	c.mu.Unlock()

	// Close detached sources without holding c.mu — a slow Close must not
	// block executeSQL / getSchema readers.
	for name, src := range oldSources {
		if err := src.Close(c.ctx); err != nil {
			log.Printf("[WARN] Failed to close source %s: %v", name, err)
		}
	}

	semanticCount := 0
	for _, sc := range configPayload.SemanticContext {
		semanticCount += len(sc.Tables) + len(sc.Glossary)
	}
	log.Printf("[INFO] Semantic context: %d source(s), %d annotation/glossary entries",
		len(configPayload.SemanticContext), semanticCount)

	// Dial new sources without holding c.mu (Connect can block on network I/O).
	newSources := make(map[string]core.Source, len(configPayload.Sources))
	for _, remoteSrc := range configPayload.Sources {
		src, err := adapter.NewSource(remoteSrc.Type, remoteSrc.DSN, remoteSrc.NoLock, remoteSrc.NormalizeTurkish)
		if err != nil {
			log.Printf("[ERROR] Failed to create source %s: %v", remoteSrc.Name, err)
			continue
		}

		if err := src.Connect(c.ctx); err != nil {
			log.Printf("[ERROR] Failed to connect to source %s: %v", remoteSrc.Name, err)
			continue
		}

		newSources[remoteSrc.Name] = src
		log.Printf("[INFO] Source synced: %s (%s) [ReadOnly: %v]", remoteSrc.Name, remoteSrc.Type, remoteSrc.ReadOnly)
	}

	// Publish the new source set in one swap.
	c.mu.Lock()
	c.sources = newSources
	c.mu.Unlock()

	log.Println("[INFO] Configuration sync completed!")
}

func (c *ConnectClient) executeSQL(sourceName, query string, params map[string]interface{}) (interface{}, error) {
	c.mu.RLock()
	src, exists := c.sources[sourceName]
	c.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("source not found: %s", sourceName)
	}

	// Validate the query before execution.
	c.mu.RLock()
	validator := c.queryValidator
	modifier := c.queryModifier
	c.mu.RUnlock()

	if err := validator.ValidateQuery(query); err != nil {
		return nil, fmt.Errorf("query validation failed: %w", err)
	}
	modifiedQuery, err := modifier.AddRowLimit(query)
	if err != nil {
		return nil, fmt.Errorf("failed to apply row limit: %w", err)
	}
	query = modifiedQuery

	log.Printf("[INFO] Executing SQL on source '%s': %s", sourceName, query)
	if len(params) > 0 {
		log.Printf("[DEBUG] Query parameters: %v", params)
	}

	// Bind params as named SQL arguments. Adapters that don't honour bind
	// parameters (REST/GraphQL/dummy) simply ignore the extra args; MSSQL
	// forwards them to database/sql, which accepts sql.NamedArg.
	args := make([]any, 0, len(params))
	for k, v := range params {
		args = append(args, sql.Named(k, v))
	}

	execCtx := c.ctx
	cancel := func() {}
	if c.commandTimeout > 0 {
		execCtx, cancel = context.WithTimeout(c.ctx, c.commandTimeout)
	}
	defer cancel()

	startTime := time.Now()
	result, err := src.ExecuteQuery(execCtx, query, args...)
	executionTime := time.Since(startTime)

	if err != nil {
		log.Printf("[ERROR] SQL execution failed after %v: %v", executionTime, err)
		return nil, err
	}

	log.Printf("[INFO] SQL executed successfully in %v", executionTime)

	// Format response with metadata
	response := map[string]interface{}{
		"data":           result,
		"execution_time": executionTime.Milliseconds(),
		"source":         sourceName,
		"timestamp":      time.Now().Unix(),
	}

	return response, nil
}

func (c *ConnectClient) getSchema(sourceName string) (interface{}, error) {
	c.mu.RLock()
	src, exists := c.sources[sourceName]
	semantic, hasSemantic := c.semanticContext[sourceName]
	c.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("source not found: %s", sourceName)
	}

	log.Printf("[INFO] Retrieving schema for source '%s'", sourceName)

	getCtx := c.ctx
	cancel := func() {}
	if c.commandTimeout > 0 {
		getCtx, cancel = context.WithTimeout(c.ctx, c.commandTimeout)
	}
	defer cancel()

	// Get schema
	startTime := time.Now()
	schema, err := src.GetSchema(getCtx)
	retrievalTime := time.Since(startTime)

	if err != nil {
		log.Printf("[ERROR] Schema retrieval failed after %v: %v", retrievalTime, err)
		return nil, err
	}

	log.Printf("[INFO] Schema retrieved successfully in %v", retrievalTime)

	// Format response with metadata. Semantic context (table/column descriptions
	// + glossary, pushed by Python control plane via config_sync) is attached so
	// the LLM-facing layer can render business meaning alongside raw schema.
	response := map[string]interface{}{
		"schema":         schema,
		"retrieval_time": retrievalTime.Milliseconds(),
		"source":         sourceName,
		"timestamp":      time.Now().Unix(),
	}
	if hasSemantic {
		response["semantic_context"] = semantic
	}

	return response, nil
}

func (c *ConnectClient) listSources() map[string]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()

	sources := make([]map[string]interface{}, 0, len(c.sources))
	for name := range c.sources {
		sources = append(sources, map[string]interface{}{
			"name":   name,
			"status": "connected",
		})
	}

	return map[string]interface{}{
		"sources": sources,
		"count":   len(sources),
	}
}

func (c *ConnectClient) sendMessage(msgType MessageType, payload interface{}) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	msg := WSMessage{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		Type:      msgType,
		Timestamp: time.Now().Unix(),
		Payload:   payloadJSON,
	}

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("connection is nil")
	}

	// Serialize writes with write mutex
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return conn.WriteJSON(msg)
}

func (c *ConnectClient) sendResponse(commandID string, data interface{}) error {
	payload := ResponsePayload{
		CommandID: commandID,
		Success:   true,
		Data:      data,
	}
	return c.sendMessage(MsgTypeResponse, payload)
}

func (c *ConnectClient) sendError(commandID, errMsg string) error {
	payload := ResponsePayload{
		CommandID: commandID,
		Success:   false,
		Error:     errMsg,
	}
	return c.sendMessage(MsgTypeError, payload)
}

func (c *ConnectClient) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.mu.RLock()
			sourceCount := len(c.sources)
			c.mu.RUnlock()

			if err := c.sendMessage(MsgTypeHeartbeat, map[string]interface{}{
				"timestamp": time.Now().Unix(),
				"sources":   sourceCount,
			}); err != nil {
				log.Printf("[WARN] Heartbeat failed: %v", err)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (c *ConnectClient) closeConnection() {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()

	if conn != nil {
		// Send close message with write mutex
		c.writeMu.Lock()
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		c.writeMu.Unlock()
		conn.Close()
	}
}

func (c *ConnectClient) cleanup() {
	// Close WebSocket connection
	c.closeConnection()

	// Close all database sources with fresh context (c.ctx may be cancelled)
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()

	c.mu.Lock()
	defer c.mu.Unlock()

	for name, src := range c.sources {
		if err := src.Close(cleanupCtx); err != nil {
			log.Printf("[WARN] Failed to close source %s: %v", name, err)
		}
	}
	c.sources = make(map[string]core.Source)
}
