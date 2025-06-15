package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func Run(ctx context.Context) error {
	logger, err := newLogger()
	if err != nil {
		return fmt.Errorf("failed to create logger: %w", err)
	}

	{
		v, cancel := rootContext(ctx, logger)
		defer cancel()

		ctx = v
	}

	logger.LogAttrs(ctx, slog.LevelInfo,
		"loading config",
	)

	cfg, err := newConfig(ctx, logger)
	if err != nil {
		const msg = "failed to load runtime config"
		logger.LogAttrs(ctx, slog.LevelError,
			msg,
			errAttr(err),
		)
		return fmt.Errorf("%s: %w", msg, err)
	}

	return run(ctx, logger, cfg)
}

func run(ctx context.Context, logger *slog.Logger, cfg config) error {
	const syncInterval = 4 * time.Hour

	logger.LogAttrs(ctx, slog.LevelInfo,
		"service initializing",
		slog.String("interval", syncInterval.String()),
	)

	hc := &http.Client{
		Timeout: 10 * time.Second,
	}
	defer hc.CloseIdleConnections()

	ticker := time.NewTicker(syncInterval)
	tickTime := time.Now()
	defer ticker.Stop()

	var consecutiveFailCount int

	syncFunc := newSyncFunc(&cfg, hc, func(r *http.Request) *http.Request {
		r.Header.Set("User-Agent", "github.com---josephcopenhaver--cloudflare-dns-sync/1.0")
		return r
	})

	if err := ctx.Err(); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn,
			"service start canceled",
			slog.String("reason", err.Error()),
		)
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			defer panic(r)
			logger.LogAttrs(ctx, slog.LevelError,
				"service exited unexpectedly",
			)
			return
		}

		logger.LogAttrs(ctx, slog.LevelWarn,
			"service stopped",
		)
	}()

	logger.LogAttrs(ctx, slog.LevelInfo,
		"service starting",
		slog.String("interval", syncInterval.String()),
	)

	doneChan := ctx.Done()
	for {
		if err := syncFunc(ctx, logger, tickTime); err != nil {
			logger.LogAttrs(ctx, slog.LevelError,
				"sync fail",
				slog.Int("consecutive_fail_count", consecutiveFailCount),
				errAttr(err),
			)

			consecutiveFailCount++
		} else {
			logger.LogAttrs(ctx, slog.LevelInfo,
				"sync ok",
			)

			consecutiveFailCount = 0
		}

		select {
		case <-doneChan:
			return nil
		default:
		}

		// play nice with the network resources we're talking to
		// and close idle connections while we wait for the next tick
		//
		// the ticks are surely going to exceed the timeout of the http.Client in all cases
		hc.CloseIdleConnections()

		select {
		case <-doneChan:
			return nil
		case tickTime = <-ticker.C:
		}
	}
}

func newSyncFunc(cfg *config, hc *http.Client, reqDeco func(*http.Request) *http.Request) func(context.Context, *slog.Logger, time.Time) error {
	apiToken := cfg.ApiToken

	var reqBodyStrPrefix string
	{
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(cfg.RecordName); err != nil {
			panic(fmt.Errorf("failed to json encode record name: %w", err))
		}

		reqBodyStrPrefix = `{"type":"A","name":` + strings.TrimSuffix(buf.String(), "\n") + `,"content":`
	}

	reqBodyStrSuffix := `,"ttl":` + strconv.Itoa(cfg.ttl) + `,"proxied":false}`

	readIPBaseReq, err := http.NewRequest(http.MethodGet, "https://checkip.amazonaws.com/", http.NoBody)
	if err != nil {
		panic(err)
	}

	setRecordBaseReq, err := http.NewRequest(http.MethodPut, "https://api.cloudflare.com/client/v4/zones/"+url.PathEscape(cfg.ZoneID)+"/dns_records/"+url.PathEscape(cfg.RecordID), http.NoBody)
	if err != nil {
		panic(fmt.Errorf("failed to create base request for setting DNS record: %w", err))
	}

	var oldIP string

	return func(ctx context.Context, logger *slog.Logger, t time.Time) error {
		logger.LogAttrs(ctx, slog.LevelInfo,
			"sync running",
			slog.Int64("tick_time", t.UnixNano()),
		)

		var ip, jsonIP string
		err := func() error {
			req := reqDeco(readIPBaseReq.Clone(ctx))
			resp, err := hc.Do(req)
			if err != nil {
				return fmt.Errorf("failed to get response from checkip.amazonaws.com: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				if _, err := io.Copy(io.Discard, req.Body); err != nil {
					return err
				}

				return errors.New("response status code from checkip.amazonaws.com is not in 2xx range: " + strconv.Itoa(resp.StatusCode))
			}

			b, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}

			ip = strings.TrimSpace(string(b))
			if net.ParseIP(ip) == nil {
				return errors.New("checkip.amazonaws.com failed to return a valid IP address in response body")
			}

			var buf bytes.Buffer
			if err := json.NewEncoder(&buf).Encode(ip); err != nil {
				return fmt.Errorf("failed to json encode ip: %w", err)
			}
			jsonIP = strings.TrimSuffix(buf.String(), "\n")

			return nil
		}()
		if err != nil {
			const msg = "failed to determine IP address"
			logger.LogAttrs(ctx, slog.LevelError,
				msg,
				errAttr(err),
			)
			return fmt.Errorf("%s: %w", msg, err)
		}

		if ip == oldIP {
			logger.LogAttrs(ctx, slog.LevelInfo,
				"same IP address",
				slog.String("ip", ip),
			)
			return nil
		}

		logger.LogAttrs(ctx, slog.LevelInfo,
			"new IP address",
			slog.String("ip_old", oldIP),
			slog.String("ip_new", ip),
		)

		err = func() error {
			req := setRecordBaseReq.Clone(ctx)
			req.Body = io.NopCloser(strings.NewReader(reqBodyStrPrefix + jsonIP + reqBodyStrSuffix))
			req.GetBody = nil

			h := req.Header
			h.Set("Content-Type", "application/json")
			h.Set("Authorization", "Bearer "+apiToken)
			req = reqDeco(req)

			resp, err := hc.Do(req)
			if err != nil {
				return fmt.Errorf("failed to get response from cloudflare api: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				const msg = "unexpected response status code from cloudflare"
				logger.LogAttrs(ctx, slog.LevelError,
					msg,
					slog.Int("status_code", resp.StatusCode),
				)

				if _, err := io.Copy(io.Discard, req.Body); err != nil {
					logger.LogAttrs(ctx, slog.LevelError,
						"failed to read full non-success response body from cloudflare",
						errAttr(err),
					)
				}

				return errors.New(msg + ": " + strconv.Itoa(resp.StatusCode))
			}

			if _, err := io.Copy(io.Discard, req.Body); err != nil {
				logger.LogAttrs(ctx, slog.LevelWarn,
					"failed to read full success response body from cloudflare",
					errAttr(err),
				)
			}

			return nil
		}()
		if err != nil {
			const msg = "failed to verify Cloudflare DNS record was updated"
			logger.LogAttrs(ctx, slog.LevelError,
				msg,
				errAttr(err),
			)
			return fmt.Errorf("%s: %w", msg, err)
		}

		oldIP = ip
		return nil
	}
}
