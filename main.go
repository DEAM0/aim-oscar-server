package main

import (
	"aim-oscar/config"
	"aim-oscar/db"
	"aim-oscar/models"
	"aim-oscar/oscar"
	"aim-oscar/services"
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/uptrace/bun/extra/bundebug"
	"golang.org/x/exp/slog"
)

func main() {
	configPath := flag.String("config", "", "Path to app config")
	flag.Parse()

	if configPath == nil || *configPath == "" {
		flag.Usage()
		os.Exit(1)
	}

	conf, err := config.FromFile(*configPath)
	if err != nil {
		log.Fatalf("could not parse config: %s", err)
	}

	var level slog.Level = slog.LevelDebug
	if err := level.UnmarshalText([]byte(conf.AppConfig.LogLevel)); err != nil {
		log.Fatalf("invalid app.log_level: %s", err)
	}

	var logHandler slog.Handler = NewOSCARLogHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	if conf.AppConfig.LogStyle == "machine" {
		logHandler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}

	logger := slog.New(logHandler)
	slog.SetDefault(logger)

	db, err := db.Connect(&conf.DBConfig)
	if err != nil {
		logger.Error("could not connect to DB", slog.String("err", err.Error()))
		os.Exit(1)
	}

	// Print all queries to stdout.
	db.AddQueryHook(bundebug.NewQueryHook(bundebug.WithVerbose(conf.AppConfig.LogLevel == slog.LevelDebug.String())))

	// Register our DB models
	db.RegisterModel((*models.User)(nil), (*models.Message)(nil), (*models.Buddy)(nil), (*models.EmailVerification)(nil))

	// On start, all users must be offline bc there are no connections (while this is a one-server operation)
	ctx := context.Background()
	if _, err := db.NewUpdate().Model(&models.User{}).Set("status = ?", models.UserStatusAway).Where("status != ?", models.UserStatusAway).Exec(ctx); err != nil {
		logger.Error("could not set all users as offline", "err", err.Error())
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", conf.OscarConfig.Addr)
	if err != nil {
		fmt.Println("Error listening: ", err.Error())
		os.Exit(1)
	}
	defer listener.Close()

	sessionManager := NewSessionManager()

	// Goroutine that listens for messages to deliver and tries to find a user socket to push them to
	commCh, messageRoutine := MessageDelivery(sessionManager, logger)
	go messageRoutine(db)

	// Goroutine that listens for users who change their online status and notifies their buddies
	onlineCh, onlineRoutine := OnlineNotification(sessionManager, logger)
	go onlineRoutine(db)

	serviceManager := NewServiceManager()
	serviceManager.RegisterService(0x01, &services.GenericServiceControls{OnlineCh: onlineCh, ServerHostname: conf.OscarConfig.Addr})
	serviceManager.RegisterService(0x02, &services.LocationServices{OnlineCh: onlineCh})
	serviceManager.RegisterService(0x03, &services.BuddyListManagement{OnlineCh: onlineCh})
	serviceManager.RegisterService(0x04, &services.ICBM{CommCh: commCh})
	// serviceManager.RegisterService(0x0f, &services.DirectorySearchService{})
	// serviceManager.RegisterService(0x13, &services.FeedbagService{})
	serviceManager.RegisterService(0x17, &services.AuthorizationRegistrationService{BOSAddress: conf.OscarConfig.BOS})
	serviceManager.RegisterService(0x18, &services.AlertService{})

	handleCloseFn := func(ctx context.Context, session *oscar.Session) {
		session.Logger.Info("Disconnected")

		user := models.UserFromContext(ctx)
		if user != nil {
			if err := user.SetAway(ctx, db); err != nil {
				logger.Error("Could not set user as away", slog.String("err", err.Error()))
			}

			logger.Info("Disconnecting user", slog.String("screen_name", user.ScreenName))

			onlineCh <- user
			if session, err := oscar.SessionFromContext(ctx); err == nil {
				session.Disconnect()
				sessionManager.RemoveSession(user.ScreenName)
			}
		}
	}

	handleFn := func(ctx context.Context, flap *oscar.FLAP) context.Context {
		session, err := oscar.SessionFromContext(ctx)
		if err != nil {
			// TODO
			logger.Error("no session in context", slog.String("flap", flap.String()))
			return ctx
		}

		if user := models.UserFromContext(ctx); user != nil {
			if conf.AppConfig.LogLevel == slog.LevelDebug.String() {
				logger.Debug("RECV",
					slog.String("screen_name", user.ScreenName),
					slog.String("ip", session.RemoteAddr().String()),
					"flap", flap,
				)
			}
			user.LastActivityAt = time.Now()
			ctx = models.NewContextWithUser(ctx, user)
			session.ScreenName = user.ScreenName
			sessionManager.SetSession(user.ScreenName, session)
		} else {
			if conf.AppConfig.LogLevel == slog.LevelDebug.String() {
				logger.Debug("RECV",
					slog.String("ip", session.RemoteAddr().String()),
					"flap", flap,
				)
			}
		}

		if flap.Header.Channel == 1 {
			// Is this a hello?
			if bytes.Equal(flap.Data.Bytes(), []byte{0, 0, 0, 1}) {
				return ctx
			}

			user, screenName, err := services.AuthenticateFLAPCookie(ctx, db, flap)
			if err != nil {
				session.Logger.Error("Could not authenticate user cookie", "screen_name", screenName, slog.String("err", err.Error()))
				return ctx
			}

			session.Logger.Info("Authenticated user", "screen_name", user.ScreenName)

			session.ScreenName = user.ScreenName
			ctx = models.NewContextWithUser(ctx, user)

			// Send available services
			servicesSnac := oscar.NewSNAC(0x1, 0x3)
			for _, service := range services.ServiceVersions {
				servicesSnac.Data.WriteUint16(service.Family)
			}

			servicesFlap := oscar.NewFLAP(2)
			servicesFlap.Data.WriteBinary(servicesSnac)
			session.Send(servicesFlap)

			return ctx
		} else if flap.Header.Channel == 2 {
			snac := &oscar.SNAC{}
			if err := snac.UnmarshalBinary(flap.Data.Bytes()); err != nil {
				session.Logger.Error("could not unmarshal FLAP data", "err", err)
				session.Disconnect()
				handleCloseFn(ctx, session)
				return ctx
			}

			if service, ok := serviceManager.GetService(snac.Header.Family); ok {
				newCtx, err := service.HandleSNAC(ctx, db, snac)
				if err != nil {
					session.Logger.Error("error handling SNAC", slog.String("err", err.Error()))
					session.Disconnect()
					handleCloseFn(ctx, session)
				}

				return newCtx
			}
		} else if flap.Header.Channel == 4 {
			handleCloseFn(ctx, session)
		} else if flap.Header.Channel == 5 {
			// User is still connected
			// TODO: handle when user stops sending these messages?
			return ctx
			// session.Logger.Debug(fmt.Sprintf("%s is still connected", session.ScreenName))
		} else {
			session.Logger.Info("unhandled channel message", "channel", flap.Header.Channel, "flap", flap)
		}

		return ctx
	}

	handler := oscar.NewHandler(handleFn, handleCloseFn)

	var metricsServer *http.Server
	if conf.AppConfig.Metrics.Addr != "" {
		mux := http.NewServeMux()
		metricsHandler := promhttp.Handler()

		if conf.AppConfig.Metrics.User != "" && conf.AppConfig.Metrics.Password != "" {
			metricsHandler = BasicAuth(promhttp.Handler().ServeHTTP, conf.AppConfig.Metrics.User, conf.AppConfig.Metrics.Password, "identify yourself")
		}

		mux.Handle("/metrics", metricsHandler)
		metricsServer = &http.Server{
			Addr:    conf.AppConfig.Metrics.Addr,
			Handler: mux,
		}
		go func() {
			logger.Info("Metrics handler started", "metrics_server_addr", metricsServer.Addr)
			metricsServer.ListenAndServe()
		}()
	}

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGABRT)
	go func() {
		<-exitChan
		close(commCh)
		close(onlineCh)

		if metricsServer != nil {
			metricsServer.Close()
		}

		logger.Info("Shutting down")
		os.Exit(1)
	}()

	logger.Info("Listening on " + conf.OscarConfig.Addr)
	logger.Info("BOS host " + conf.OscarConfig.BOS)
	for {
		conn, err := listener.Accept()
		if err != nil {
			logger.Error("error accepting connection: ", err.Error())
			os.Exit(1)
		}

		go handler.Handle(conn, logger)
	}
}
