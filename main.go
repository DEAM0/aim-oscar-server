package main

import (
	"aim-oscar/models"
	"aim-oscar/oscar"
	"aim-oscar/util"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/errors"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dbfixture"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/sqliteshim"
	"github.com/uptrace/bun/extra/bundebug"
)

const (
	SRV_HOST    = "10.0.1.2"
	SRV_PORT    = "5190"
	SRV_ADDRESS = SRV_HOST + ":" + SRV_PORT
)

var services map[uint16]Service

// Map username to session
var sessions map[string]*oscar.Session
var sessionsMutex = &sync.RWMutex{}

func getSession(username string) *oscar.Session {
	sessionsMutex.RLock()
	s, ok := sessions[username]
	sessionsMutex.RUnlock()

	if ok {
		return s
	}
	return nil
}

func init() {
	services = make(map[uint16]Service)
	sessions = make(map[string]*oscar.Session)
}

func RegisterService(family uint16, service Service) {
	services[family] = service
}

func main() {
	// Set up the DB
	sqldb, err := sql.Open(sqliteshim.ShimName, "file:aim.db")
	if err != nil {
		panic(err)
	}
	db := bun.NewDB(sqldb, sqlitedialect.New())
	db.SetConnMaxIdleTime(15 * time.Second)
	db.SetConnMaxLifetime(1 * time.Minute)

	// Print all queries to stdout.
	db.AddQueryHook(bundebug.NewQueryHook(bundebug.WithVerbose(true)))

	// Register our DB models
	db.RegisterModel((*models.User)(nil), (*models.Message)(nil), (*models.Buddy)(nil))

	// dev: load in fixtures to test against
	fixture := dbfixture.New(db, dbfixture.WithRecreateTables())
	err = fixture.Load(context.Background(), os.DirFS("models"), "fixtures.yml")
	if err != nil {
		panic(err)
	}

	listener, err := net.Listen("tcp", SRV_ADDRESS)
	if err != nil {
		fmt.Println("Error listening: ", err.Error())
		os.Exit(1)
	}
	defer listener.Close()

	// Goroutine that listens for messages to deliver and tries to find a user socket to push them to
	commCh, messageRoutine := MessageDelivery()
	go messageRoutine(db)

	// Goroutine that listens for users who change their online status and notifies their buddies
	onlineCh, onlineRoutine := OnlineNotification()
	go onlineRoutine(db)

	handleCloseFn := func(ctx context.Context, session *oscar.Session) {
		log.Printf("%v disconnected", session.RemoteAddr())

		user := models.UserFromContext(ctx)
		if user != nil {
			// tellBuddies := user.Status != models.UserStatusAway
			user.Status = models.UserStatusAway
			if err := user.Update(ctx, db); err != nil {
				log.Print(errors.Wrap(err, "could not set user as inactive"))
			}

			if true {
				onlineCh <- user
			}
		}
	}

	handleFn := func(ctx context.Context, flap *oscar.FLAP) context.Context {
		session, err := oscar.SessionFromContext(ctx)
		if err != nil {
			util.PanicIfError(err)
		}

		if user := models.UserFromContext(ctx); user != nil {
			fmt.Printf("%s (%v) ->\n%+v\n", user.Username, session.RemoteAddr(), flap)
			user.LastActivityAt = time.Now()
			ctx = models.NewContextWithUser(ctx, user)
			sessions[user.Username] = session
		} else {
			fmt.Printf("%v ->\n%+v\n", session.RemoteAddr(), flap)
		}

		if flap.Header.Channel == 1 {
			// Is this a hello?
			if bytes.Equal(flap.Data.Bytes(), []byte{0, 0, 0, 1}) {
				return ctx
			}

			user, err := AuthenticateFLAPCookie(ctx, db, flap)
			if err != nil {
				log.Printf("Could not authenticate cookie: %s", err)
				return ctx
			}
			ctx = models.NewContextWithUser(ctx, user)

			// Send available services
			servicesSnac := oscar.NewSNAC(1, 3)
			for family := range versions {
				servicesSnac.Data.WriteUint16(family)
			}

			servicesFlap := oscar.NewFLAP(2)
			servicesFlap.Data.WriteBinary(servicesSnac)
			session.Send(servicesFlap)

			return ctx
		} else if flap.Header.Channel == 2 {
			snac := &oscar.SNAC{}
			err := snac.UnmarshalBinary(flap.Data.Bytes())
			util.PanicIfError(err)

			fmt.Printf("%+v\n", snac)
			if tlvs, err := oscar.UnmarshalTLVs(snac.Data.Bytes()); err == nil {
				for _, tlv := range tlvs {
					fmt.Printf("%+v\n", tlv)
				}
			} else {
				fmt.Printf("%s\n\n", util.PrettyBytes(snac.Data.Bytes()))
			}

			if service, ok := services[snac.Header.Family]; ok {
				newCtx, err := service.HandleSNAC(ctx, db, snac)
				util.PanicIfError(err)
				return newCtx
			}
		} else if flap.Header.Channel == 4 {
			session.Disconnect()
			handleCloseFn(ctx, session)
		}

		return ctx
	}

	handler := oscar.NewHandler(handleFn, handleCloseFn)

	RegisterService(0x01, &GenericServiceControls{OnlineCh: onlineCh})
	RegisterService(0x02, &LocationServices{OnlineCh: onlineCh})
	RegisterService(0x03, &BuddyListManagement{})
	RegisterService(0x04, &ICBM{CommCh: commCh})
	RegisterService(0x17, &AuthorizationRegistrationService{})

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGABRT)
	go func() {
		<-exitChan
		close(commCh)
		close(onlineCh)
		fmt.Println("Shutting down")
		os.Exit(1)
	}()

	fmt.Println("OSCAR listening on " + SRV_ADDRESS)
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Error accepting connection: ", err.Error())
			os.Exit(1)
		}

		log.Printf("Connection from %v", conn.RemoteAddr())
		go handler.Handle(conn)
	}
}
