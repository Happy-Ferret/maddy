package maddy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-smtp"
	"github.com/emersion/maddy/config"
	"github.com/emersion/maddy/log"
	"github.com/emersion/maddy/module"
	sqlstore "github.com/foxcpp/go-imap-sql"
	imapsql "github.com/foxcpp/go-imap-sql/imap"
)

type SQLStorage struct {
	*imapsql.Backend
	instName string
	Log      log.Logger
}

type Literal struct {
	io.Reader
	length int
}

func (l Literal) Len() int {
	return l.length
}

func (sqlm *SQLStorage) Name() string {
	return "sql"
}

func (sqlm *SQLStorage) InstanceName() string {
	return sqlm.instName
}

func NewSQLStorage(_, instName string) (module.Module, error) {
	return &SQLStorage{
		instName: instName,
		Log:      log.Logger{Name: "sql"},
	}, nil
}

func (sqlm *SQLStorage) Init(cfg *config.Map) error {
	var driver string
	var dsn string
	appendlimitVal := int64(-1)

	opts := imapsql.Opts{}
	cfg.String("driver", false, true, "", &driver)
	cfg.String("dsn", false, true, "", &dsn)
	cfg.Int64("appendlimit", false, false, 32*1024*1024, &appendlimitVal)
	cfg.Bool("debug", true, &sqlm.Log.Debug)

	if _, err := cfg.Process(); err != nil {
		return err
	}

	if appendlimitVal == -1 {
		opts.MaxMsgBytes = nil
	} else {
		opts.MaxMsgBytes = new(uint32)
		*opts.MaxMsgBytes = uint32(appendlimitVal)
	}
	back, err := imapsql.NewBackend(driver, dsn, opts)
	if err != nil {
		return fmt.Errorf("sql: %s", err)
	}
	sqlm.Backend = back

	sqlm.Log.Debugln("go-imap-sql version", sqlstore.VersionStr)

	return nil
}

func (sqlm *SQLStorage) IMAPExtensions() []string {
	return []string{"APPENDLIMIT", "MOVE", "CHILDREN"}
}

func (sqlm *SQLStorage) Deliver(ctx module.DeliveryContext, msg io.Reader) error {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, msg); err != nil {
		return err
	}
	for _, rcpt := range ctx.To {
		parts := strings.Split(rcpt, "@")
		if len(parts) != 2 {
			sqlm.Log.Println("malformed address:", rcpt)
			return errors.New("Deliver: missing domain part")
		}

		u, err := sqlm.GetExistingUser(parts[0])
		if err != nil {
			sqlm.Log.Printf("failed to get user for %s (delivery ID = %s): %v", rcpt, ctx.DeliveryID, err)
			if err == sqlstore.ErrUserDoesntExists {
				return &smtp.SMTPError{
					Code:    550,
					Message: "Local mailbox doesn't exists",
				}
			}
			return err
		}

		// TODO: We need to handle Ctx["spam"] here.
		tgtMbox := "INBOX"

		mbox, err := u.GetMailbox(tgtMbox)
		if err != nil {
			if err == backend.ErrNoSuchMailbox {
				// Create INBOX if it doesn't exists.
				sqlm.Log.Debugln("creating inbox for", rcpt)
				if err := u.CreateMailbox(tgtMbox); err != nil {
					sqlm.Log.Debugln("inbox creation failed for", rcpt)
					return err
				}
				mbox, err = u.GetMailbox(tgtMbox)
				if err != nil {
					return err
				}
			} else {
				sqlm.Log.Printf("failed to get inbox for %s (delivery ID = %s): %v", rcpt, ctx.DeliveryID, err)
				return err
			}
		}

		headerPrefix := fmt.Sprintf("Return-Path: <%s>\r\n", sanitizeString(ctx.From))
		headerPrefix += fmt.Sprintf("Delivered-To: %s\r\n", sanitizeString(rcpt))

		msg := Literal{
			Reader: io.MultiReader(strings.NewReader(headerPrefix), &buf),
			length: len(headerPrefix) + buf.Len(),
		}
		if err := mbox.CreateMessage([]string{}, time.Now(), msg); err != nil {
			sqlm.Log.Printf("failed to save msg for %s (delivery ID = %s): %v", rcpt, ctx.DeliveryID, err)
			return err
		}
	}
	return nil
}

func init() {
	module.Register("sql", NewSQLStorage)
}
