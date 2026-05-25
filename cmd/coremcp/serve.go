package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/corebasehq/coremcp/pkg/adapter"
	"github.com/corebasehq/coremcp/pkg/config"
	"github.com/corebasehq/coremcp/pkg/core"
	"github.com/corebasehq/coremcp/pkg/security"
	"github.com/corebasehq/coremcp/pkg/server"
	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Starts the MCP server",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.SetOutput(os.Stderr)

		if cfg == nil {
			return fmt.Errorf("failed to load configuration")
		}

		fmt.Fprintf(os.Stderr, "Starting CoreMCP Server via %s...\n", cfg.Server.Transport)

		// Default to the version compiled in via -ldflags (cmd/coremcp/version.go).
		// Yaml-supplied server.version is still honoured if explicitly set, but
		// users no longer have to bump it by hand on every release.
		serverVersion := cfg.Server.Version
		if serverVersion == "" {
			serverVersion = Version
		}
		mcpSrv := server.NewMCPServer(cfg.Server.Name, serverVersion)

		// Configure security features
		log.Println("Configuring security features...")
		piiPatterns := convertPIIPatterns(cfg.Security.PIIPatterns)
		if err := mcpSrv.ConfigureSecurity(
			cfg.Security.MaxRowLimit,
			cfg.Security.EnablePIIMasking,
			piiPatterns,
			cfg.Security.AllowedKeywords,
			cfg.Security.BlockedKeywords,
		); err != nil {
			return fmt.Errorf("failed to configure security: %w", err)
		}
		log.Printf("Security configured: MaxRowLimit=%d, PIIMasking=%v",
			cfg.Security.MaxRowLimit, cfg.Security.EnablePIIMasking)

		connectedSources := make(map[string]core.Source)
		var schemaWG sync.WaitGroup
		defer func() {
			schemaWG.Wait()
			if len(connectedSources) == 0 {
				return
			}
			perSourceTimeout := 5 * time.Second
			var closeWG sync.WaitGroup
			for name, src := range connectedSources {
				closeWG.Add(1)
				go func(name string, src core.Source) {
					defer closeWG.Done()
					cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), perSourceTimeout)
					defer cleanupCancel()
					if err := src.Close(cleanupCtx); err != nil {
						log.Printf("WARNING: Failed to close source %s: %v", name, err)
					}
				}(name, src)
			}
			closeWG.Wait()
		}()

		for _, sourceCfg := range cfg.Sources {
			src, err := adapter.NewSource(sourceCfg.Type, sourceCfg.BuildDSN(), sourceCfg.NoLock, sourceCfg.NormalizeTurkish)
			if err != nil {
				log.Printf("ERROR: Failed to create source %s: %v\n", sourceCfg.Name, err)
				continue
			}

			if err := src.Connect(cmd.Context()); err != nil {
				log.Printf("ERROR: Failed to connect to database %s: %v — skipping source", sourceCfg.Name, err)
				continue
			}
			connectedSources[sourceCfg.Name] = src

			mcpSrv.AddSource(sourceCfg.Name, src, sourceCfg.IsReadOnly())
			log.Printf("Source ready: %s (%s) [ReadOnly: %v, NoLock: %v, NormalizeTurkish: %v]", sourceCfg.Name, sourceCfg.Type, sourceCfg.IsReadOnly(), sourceCfg.NoLock, sourceCfg.NormalizeTurkish)
		}

		// Load database schemas for AI context in background
		// so the MCP server can respond to initialize immediately
		schemaWG.Add(1)
		go func() {
			defer schemaWG.Done()
			log.Println("Loading database schemas for AI context (background)...")
			if err := mcpSrv.LoadSchemas(cmd.Context()); err != nil {
				log.Printf("WARNING: Failed to load schemas: %v", err)
			} else {
				log.Println("Database schemas loaded successfully!")
			}
		}()

		// Register custom tools from config
		if len(cfg.CustomTools) > 0 {
			log.Printf("Registering %d custom tool(s)...", len(cfg.CustomTools))
			for _, toolCfg := range cfg.CustomTools {
				params := make([]server.ToolParam, len(toolCfg.Parameters))
				for i, p := range toolCfg.Parameters {
					params[i] = server.ToolParam{
						Name:     p.Name,
						Type:     p.Type,
						Required: p.Required,
						Default:  p.Default,
					}
				}

				if err := mcpSrv.AddCustomTool(
					toolCfg.Name,
					toolCfg.Description,
					toolCfg.Source,
					toolCfg.Query,
					params,
				); err != nil {
					log.Printf("WARNING: Failed to register custom tool %s: %v", toolCfg.Name, err)
				} else {
					log.Printf("Custom tool registered: %s", toolCfg.Name)
				}
			}
		}

		transport, _ := cmd.Flags().GetString("transport")
		if transport == "stdio" {
			log.Println("CoreMCP started on Stdio. Waiting for MCP client...")
			if err := mcpSrv.StartStdio(); err != nil {
				return err
			}
			return nil
		} else {
			return fmt.Errorf("http transport is not supported yet")
		}
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)

	serveCmd.Flags().StringP("transport", "t", "stdio", "Transport type: stdio or http")
}

// convertPIIPatterns converts config PII patterns to security PII patterns.
func convertPIIPatterns(configPatterns []config.PIIMaskPattern) []security.MaskPattern {
	patterns := make([]security.MaskPattern, len(configPatterns))
	for i, p := range configPatterns {
		patterns[i] = security.MaskPattern{
			Name:        p.Name,
			Pattern:     p.Pattern,
			Replacement: p.Replacement,
			Enabled:     p.Enabled,
		}
	}
	return patterns
}
