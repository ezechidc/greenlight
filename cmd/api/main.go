package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"github.com/ezechidc/greenlight/internal/data"
	"github.com/ezechidc/greenlight/internal/mailer"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"log"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

const version = "1.0.0"

type config struct {
	port int
	env  string
	db   struct {
		dsn          string
		maxOpenConns int
		maxIdleConns int
		maxIdleTime  time.Duration
	}
	limiter struct {
		rps     float64
		burst   int
		enabled bool
	}
	smtp struct {
		host     string
		port     int
		username string
		password string
		sender   string
	}
}

type application struct {
	config config
	logger *slog.Logger
	models data.Models
	mailer *mailer.Mailer
	wg     sync.WaitGroup
}

type FlatSourceHandler struct {
	slog.Handler
}

func (h *FlatSourceHandler) Handle(ctx context.Context, r slog.Record) error {
	// Use the PC from the record, not runtime.Caller()
	pc := r.PC
	file := ""
	line := 0
	if pc != 0 {
		fn := runtime.FuncForPC(pc)
		if fn != nil {
			file, line = fn.FileLine(pc)
		}
	}
	fileName := file
	if file != "" {
		parts := strings.Split(file, "/")
		fileName = parts[len(parts)-1]
	}
	srcStr := fmt.Sprintf("%s:%d", fileName, line)

	newRec := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	newRec.AddAttrs(slog.String("source", srcStr))
	r.Attrs(func(a slog.Attr) bool {
		if a.Key != "source" {
			newRec.AddAttrs(a)
		}
		return true
	})
	return h.Handler.Handle(ctx, newRec)
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Print(".env file not found or failed to load")
	}
	var cfg config

	flag.IntVar(&cfg.port, "port", 4000, "API server port")
	flag.StringVar(&cfg.env, "env", "development", "Environment (development|staging|production)")
	flag.StringVar(&cfg.db.dsn, "db-dsn", os.Getenv("GREENLIGHT_DB_DSN"), "PostgreSQL DSN")
	flag.IntVar(&cfg.db.maxOpenConns, "db-max-open-conns", 25, "PostgreSQL max open connections")
	flag.IntVar(&cfg.db.maxIdleConns, "db-max-idle-conns", 25, "PostgreSQL max idle connections")
	flag.DurationVar(&cfg.db.maxIdleTime, "db-max-idle-time", 15*time.Minute, "PostgreSQL max connection idle time")

	flag.Float64Var(&cfg.limiter.rps, "limiter-rps", 2, "Rate limiter maximum requests per second")
	flag.IntVar(&cfg.limiter.burst, "limiter-burst", 4, "Rate limiter maximum burst")
	flag.BoolVar(&cfg.limiter.enabled, "limiter-enabled", true, "Enable rate limiter")

	flag.StringVar(&cfg.smtp.host, "smtp-host", "sandbox.smtp.mailtrap.io", "SMTP host")
	flag.IntVar(&cfg.smtp.port, "smtp-port", 2525, "SMTP port")
	flag.StringVar(&cfg.smtp.username, "smtp-username", os.Getenv("MAIL_TRAP_USERNAME"), "SMTP username")
	flag.StringVar(&cfg.smtp.password, "smtp-password", os.Getenv("MAIL_TRAP_PASSWORD"), "SMTP password")
	flag.StringVar(&cfg.smtp.sender, "smtp-sender", "Greenlight <no-reply@denco.greenlight.net>", "SMTP sender")

	flag.Parse()
	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{})
	logger := slog.New(&FlatSourceHandler{Handler: base})

	db, err := openDB(cfg)
	if err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}

	defer db.Close()
	logger.Info("database connection pool established")
	mailerApp, err := mailer.New(cfg.smtp.host, cfg.smtp.port, cfg.smtp.username, cfg.smtp.password, cfg.smtp.sender)
	app := &application{
		config: cfg,
		logger: logger,
		models: data.NewModels(db),
		mailer: mailerApp,
	}

	err = app.serve()
	if err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}
}

func openDB(cfg config) (*sql.DB, error) {
	db, err := sql.Open("postgres", cfg.db.dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.db.maxOpenConns)
	db.SetMaxIdleConns(cfg.db.maxIdleConns)
	db.SetConnMaxLifetime(cfg.db.maxIdleTime)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = db.PingContext(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
