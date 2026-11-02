// Command app runs one part of Smart Campaign Composer, picked by its first
// argument. Everything is configured from the environment (.env.example).
//
//	api           the REST + SSE API, the web UI (WEB_DIR) and /metrics; it
//	              also ends turns orphaned by a crash (stale heartbeat)
//	worker        sends approved deliveries through the provider within the
//	              shared channel limits; /metrics and /healthz on
//	              WORKER_METRICS_ADDR
//	mockprovider  the fake SMS/email provider: nothing leaves it
//	migrate       applies the database migrations
//	seed          loads fake recipients into an empty database
//	history       adds six weeks of past demo campaigns (chats, approvals) to
//	              a seeded database once, waits for the worker to send them
//	              through the provider and moves the send times to their days
//	probe URL     exits 0 when URL answers 2xx (container health checks:
//	              the runtime image has no shell)
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // the runtime image has no zone database; APP_TIMEZONE needs one

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/chat/turns"
	"smart-campaign-composer/backend/internal/clock"
	"smart-campaign-composer/backend/internal/delivery"
	"smart-campaign-composer/backend/internal/httpapi"
	"smart-campaign-composer/backend/internal/llm"
	llmprovider "smart-campaign-composer/backend/internal/llm/provider"
	"smart-campaign-composer/backend/internal/metrics"
	"smart-campaign-composer/backend/internal/mockprovider"
	"smart-campaign-composer/backend/internal/provider"
	"smart-campaign-composer/backend/internal/seed"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/store"
)

const (
	shutdownTimeout = 15 * time.Second
	turnReapEvery   = 10 * time.Second
	dbWait          = time.Minute
	historyWait     = 15 * time.Minute // for the worker to send the demo history
)

var modes = map[string]func(context.Context, config) error{
	"api": runAPI, "worker": runWorker, "mockprovider": runMockProvider, "migrate": runMigrate, "seed": runSeed,
	"history": runHistory,
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	mode := os.Args[1]
	if mode == "probe" {
		if err := probe(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "probe:", err)
			os.Exit(1)
		}
		return
	}
	run, ok := modes[mode]
	if !ok {
		usage()
	}
	cfg, err := loadConfig(os.Getenv)
	if err == nil {
		err = cfg.require(mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})).With("mode", mode))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		slog.Error("stopped", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: app api | worker | mockprovider | migrate | seed | history | probe URL")
	os.Exit(2)
}

func runAPI(ctx context.Context, cfg config) error {
	pool, err := openPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	mainLLM, err := llmprovider.Select(cfg.LLM)
	if err != nil {
		return err
	}
	routerLLM := llm.Client(mainLLM) // the demo model plays the router in mock mode
	if mainLLM.Info().Provider == "anthropic" {
		if routerLLM, err = llmprovider.Select(cfg.Router); err != nil {
			return err
		}
	}
	info := mainLLM.Info()
	slog.Info("llm", "provider", info.Provider, "model", info.Model, "reason", info.Reason, "router_model", routerLLM.Info().Model)

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := metrics.New(reg)
	reg.MustRegister(metrics.NewDBCollector(pool, clock.Real{}))

	campaigns := campaign.NewStore(pool)
	catalog := segment.DefaultCatalog()
	runner := turns.NewRunner(turns.Deps{Pool: pool, LLM: mainLLM, Router: routerLLM, Campaigns: campaigns,
		Catalog: catalog, Clock: clock.Real{}, Metrics: m})
	api := httpapi.New(httpapi.Deps{Pool: pool, Runner: runner, Campaigns: campaigns, Catalog: catalog, Clock: clock.Real{},
		LLM: info, Router: routerLLM.Info(), TurnTimeout: cfg.TurnTimeout, WriteTimeout: cfg.SSEWriteTimeout,
		Location: cfg.Location, Metrics: m, Gatherer: reg})
	var h http.Handler = api
	if cfg.WebDir != "" {
		h = withWeb(api, cfg.WebDir)
	}

	go reapTurns(ctx, runner, cfg.TurnStaleAfter)
	// No ReadTimeout: it would cut long event streams. Streams bound each
	// write themselves (SSE_WRITE_TIMEOUT); WriteTimeout bounds the rest.
	err = serve(ctx, &http.Server{Addr: cfg.HTTPAddr, Handler: h,
		ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute})

	// Running turns go on after their streams close; let them store their
	// result (each is bounded by TURN_TIMEOUT) instead of leaving it to the
	// reaper to end them as interrupted.
	done := make(chan struct{})
	go func() { api.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(cfg.TurnTimeout + shutdownTimeout):
		slog.Warn("turns still running at exit; the reaper will end them as interrupted")
	}
	return err
}

// reapTurns ends turns whose process died (stale heartbeat) as interrupted,
// so that their chat unlocks and GET shows the outcome.
func reapTurns(ctx context.Context, r *turns.Runner, staleAfter time.Duration) {
	t := time.NewTicker(turnReapEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := r.ReapStale(ctx, staleAfter)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Error("reap stale turns", "err", err)
		case n > 0:
			slog.Warn("ended turns orphaned by a crash", "count", n)
		}
	}
}

func runWorker(ctx context.Context, cfg config) error {
	pool, err := openPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := metrics.New(reg)
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	go func() {
		if err := serve(ctx, &http.Server{Addr: cfg.WorkerMetricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}); err != nil {
			slog.Error("worker metrics server", "err", err)
		}
	}()

	w := &delivery.Worker{
		Store:    delivery.NewStore(pool, delivery.DefaultPolicy),
		Provider: provider.NewClient(cfg.ProviderURL, provider.Options{}),
		Channels: map[string]delivery.Rate{
			"sms":   {Calls: cfg.SMSRate, Window: time.Second},
			"email": {Calls: cfg.EmailRate, Window: time.Second},
		},
		Clock: clock.Real{}, Config: delivery.DefaultConfig, Name: cfg.WorkerName, Metrics: m,
	}
	slog.Info("worker", "provider", cfg.ProviderURL, "sms_per_second", cfg.SMSRate, "email_per_second", cfg.EmailRate)
	return w.Run(ctx)
}

func runMockProvider(ctx context.Context, cfg config) error {
	var faults mockprovider.Faults = mockprovider.Script{}
	if len(cfg.MockFaults) > 0 {
		r, err := mockprovider.NewRandom(cfg.MockSeed, cfg.MockFaults)
		if err != nil {
			return err
		}
		faults = r
	}
	p, err := mockprovider.OpenWithDelay(cfg.MockLog, faults, cfg.MockDelay)
	if err != nil {
		return err
	}
	defer p.Close()
	slog.Info("mock provider", "log", cfg.MockLog, "faults", cfg.MockFaults, "delay", cfg.MockDelay)
	return serve(ctx, &http.Server{Addr: cfg.MockAddr, Handler: p, ReadHeaderTimeout: 10 * time.Second})
}

func runMigrate(ctx context.Context, cfg config) error {
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := waitDB(ctx, db.PingContext); err != nil {
		return err
	}
	if err := store.Migrate(ctx, db); err != nil {
		return err
	}
	slog.Info("migrations applied")
	return nil
}

func runSeed(ctx context.Context, cfg config) error {
	pool, err := openPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	d := seed.Generate(cfg.SeedUsers, cfg.SeedRandom, time.Now())
	inserted, err := seed.Insert(ctx, pool, d)
	switch {
	case err != nil:
		return err
	case inserted:
		slog.Info("seeded fake recipients", "users", len(d.Users), "events", len(d.Events))
	default:
		slog.Info("the database already has users; seed skipped")
	}
	// The language users reach existing databases too; it adds only the
	// missing ones and changes no row.
	added, err := seed.InsertLanguageUsers(ctx, pool, time.Now())
	if err != nil {
		return err
	}
	slog.Info("language users", "added", added)
	return nil
}

// runHistory loads the demo history, waits for the worker to send it
// through the provider and then moves the recorded times to the approval
// days. Run again, it finishes a history whose sending was interrupted.
func runHistory(ctx context.Context, cfg config) error {
	pool, err := openPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	added, err := seed.InsertHistory(ctx, pool, cfg.Location, time.Now())
	switch {
	case err != nil:
		return err
	case added:
		slog.Info("added the demo history; the worker is sending it")
	default:
		slog.Info("the database has the demo history already")
	}
	deadline, logged := time.Now().Add(historyWait), time.Time{}
	for {
		left, err := seed.HistoryLeft(ctx, pool)
		if err != nil {
			return err
		}
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("history: %d deliveries or campaigns still open after %v; is the worker running?", left, historyWait)
		}
		if time.Since(logged) >= 10*time.Second {
			slog.Info("waiting for the worker", "open", left)
			logged = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	shifted, err := seed.ShiftHistory(ctx, pool)
	if err != nil {
		return err
	}
	slog.Info("demo history sent and moved to its days", "campaigns", shifted)
	return nil
}

// serve runs srv until ctx ends, then shuts it down. Requests share ctx,
// so open event streams end at shutdown instead of holding it up.
func serve(ctx context.Context, srv *http.Server) error {
	srv.BaseContext = func(net.Listener) context.Context { return ctx }
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("listening", "addr", srv.Addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return err
	}
	return nil
}

// openPool connects, waiting for the database to come up.
func openPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := waitDB(ctx, pool.Ping); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func waitDB(ctx context.Context, ping func(context.Context) error) error {
	deadline := time.Now().Add(dbWait)
	for {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := ping(pctx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("database unreachable: %w", err)
		}
		slog.Info("waiting for the database", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// probe checks that url answers 2xx.
func probe(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: app probe URL")
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(args[0])
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s answered %s", args[0], resp.Status)
	}
	return nil
}
