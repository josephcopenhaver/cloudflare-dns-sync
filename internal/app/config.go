package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strconv"
)

type config struct {
	ZoneID     string `json:"zone_id"`
	RecordID   string `json:"record_id"`
	RecordName string `json:"record_name"`
	ApiToken   string `json:"api_token"`
	TTLPtr     *int   `json:"ttl"`
	ttl        int
	noCfgFile  bool
}

func (c *config) isEmpty() bool {
	return (*c == config{})
}

func (c *config) load(ctx context.Context, logger *slog.Logger, cfgFile string) error {
	var result config

	var noCfgFile, emptyCfgFile bool

	err := func() error {
		f, err := os.Open(cfgFile)
		if err != nil {
			if os.IsNotExist(err) {
				noCfgFile = true
				return nil
			}
			return err
		}
		defer f.Close()

		if err := json.NewDecoder(f).Decode(&result); err != nil {
			return err
		}

		return nil
	}()
	if err != nil {
		const msg = "failed to load config file"
		logger.LogAttrs(ctx, slog.LevelError,
			msg,
			errAttr(err),
		)
		return fmt.Errorf("%s: %w", msg, err)
	}

	if !noCfgFile {
		emptyCfgFile = result.isEmpty()
	}

	if result.ZoneID == "" {
		if v, ok := os.LookupEnv("CLOUDFLARE_ZONE_ID"); ok && v != "" {
			result.ZoneID = v
		}
	}

	if result.RecordID == "" {
		if v, ok := os.LookupEnv("CLOUDFLARE_RECORD_ID"); ok && v != "" {
			result.RecordID = v
		}
	}

	if result.RecordName == "" {
		if v, ok := os.LookupEnv("CLOUDFLARE_RECORD_NAME"); ok && v != "" {
			result.RecordName = v
		}
	}

	if s, ok := os.LookupEnv("CLOUDFLARE_RECORD_TTL"); ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || strconv.Itoa(v) != s {
			msg := "failed to parse CLOUDFLARE_RECORD_TTL environment variable"
			logger.LogAttrs(ctx, slog.LevelError,
				msg,
				errAttr(err),
			)
			return fmt.Errorf("%s: %w", msg, err)
		}

		result.ttl = v
	} else if result.TTLPtr == nil {
		result.ttl = 120
	} else {
		result.ttl = *result.TTLPtr
	}

	if result.ApiToken == "" {
		if v, ok := os.LookupEnv("CLOUDFLARE_API_TOKEN"); ok && v != "" {
			result.ApiToken = v
		}
	}

	if result.isEmpty() {
		if noCfgFile {
			const msg = "no configuration"
			logger.LogAttrs(ctx, slog.LevelError,
				msg,
				slog.String("status", "no config file found and no environment variables set"),
			)
			return errors.New(msg)
		} else if emptyCfgFile {
			const msg = "no configuration"
			logger.LogAttrs(ctx, slog.LevelError,
				msg,
				slog.String("status", "config file is empty and no environment variables set"),
			)
			return errors.New(msg)
		}
	}

	result.noCfgFile = noCfgFile

	*c = result
	return nil
}

func (c *config) validate(ctx context.Context, logger *slog.Logger) (_err error) {
	defer func() {
		if _err == nil || !c.noCfgFile {
			return
		}

		if c.noCfgFile {
			logger.LogAttrs(ctx, slog.LevelError,
				"configuration validation failed",
				slog.String("note", "no config file found"),
				errAttr(_err),
			)
			return
		}

		logger.LogAttrs(ctx, slog.LevelError,
			"configuration validation failed",
			errAttr(_err),
		)
	}()

	if c.ApiToken == "" {
		return fmt.Errorf("CLOUDFLARE_API_TOKEN is required")
	}

	if c.ZoneID == "" {
		return fmt.Errorf("CLOUDFLARE_ZONE_ID is required")
	}

	if c.RecordID == "" {
		return fmt.Errorf("CLOUDFLARE_RECORD_ID is required")
	}

	if c.RecordName == "" {
		return fmt.Errorf("CLOUDFLARE_RECORD_NAME is required")
	}

	if c.ttl < 1 {
		return fmt.Errorf("CLOUDFLARE_RECORD_TTL must be greater than 0")
	}

	return nil
}

func (c *config) Load(ctx context.Context, logger *slog.Logger, cfgFile string) error {
	if err := c.load(ctx, logger, cfgFile); err != nil {
		return err
	}

	if err := c.validate(ctx, logger); err != nil {
		return err
	}

	return nil
}

func newConfig(ctx context.Context, logger *slog.Logger) (config, error) {
	var v, result config

	var stateDir string
	if v, ok := os.LookupEnv("CONFIG_DIR"); ok && v != "" {
		stateDir = v
	}

	cfgFile := "config.json"
	if stateDir != "" && stateDir != "." {
		cfgFile = path.Join(stateDir, cfgFile)
	}

	if err := v.Load(ctx, logger, cfgFile); err != nil {
		return result, err
	}

	result = v
	return result, nil
}
