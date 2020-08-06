package mailer

import (
	"context"
	"fmt"
	"io"
	"net/textproto"

	log "github.com/gophish/gophish/logger"
	"github.com/sirupsen/logrus"
	"github.com/soxfmr/gomail"
)

// MaxReconnectAttempts is the maximum number of times we should reconnect to a server
var MaxReconnectAttempts = 10

// ErrMaxConnectAttempts is thrown when the maximum number of reconnect attempts
// is reached.
type ErrMaxConnectAttempts struct {
	underlyingError error
}

type MailJob struct {
	Bcc   bool
	Mails []Mail
}

// Error returns the wrapped error response
func (e *ErrMaxConnectAttempts) Error() string {
	errString := "Max connection attempts exceeded"
	if e.underlyingError != nil {
		errString = fmt.Sprintf("%s - %s", errString, e.underlyingError.Error())
	}
	return errString
}

// Mailer is an interface that defines an object used to queue and
// send mailer.Mail instances.
type Mailer interface {
	Start(ctx context.Context)
	Queue(MailJob)
}

// Sender exposes the common operations required for sending email.
type Sender interface {
	Send(from string, to []string, msg io.WriterTo) error
	Close() error
	Reset() error
}

// Dialer dials to an SMTP server and returns the SendCloser
type Dialer interface {
	Dial() (Sender, error)
}

// Mail is an interface that handles the common operations for email messages
type Mail interface {
	Backoff(reason error) error
	Error(err error) error
	Success() error
	Generate(msg *gomail.Message) error
	GetDialer() (Dialer, error)
	GetRecipient() (string, error)
}

// MailWorker is the worker that receives slices of emails
// on a channel to send. It's assumed that every slice of emails received is meant
// to be sent to the same server.
type MailWorker struct {
	queue chan MailJob
}

// NewMailWorker returns an instance of MailWorker with the mail queue
// initialized.
func NewMailWorker() *MailWorker {
	return &MailWorker{
		queue: make(chan MailJob),
	}
}

// Start launches the mail worker to begin listening on the Queue channel
// for new slices of Mail instances to process.
func (mw *MailWorker) Start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-mw.queue:
			go func(ctx context.Context, job MailJob) {
				dialer, err := job.Mails[0].GetDialer()
				if err != nil {
					errorMail(err, job.Mails)
					return
				}
				sendMail(ctx, dialer, job)
			}(ctx, job)
		}
	}
}

// Queue sends the provided mail to the internal queue for processing.
func (mw *MailWorker) Queue(job MailJob) {
	mw.queue <- job
}

// errorMail is a helper to handle erroring out a slice of Mail instances
// in the case that an unrecoverable error occurs.
func errorMail(err error, ms []Mail) {
	for _, m := range ms {
		m.Error(err)
	}
}

func backoffMail(err error, ms []Mail) {
	for _, m := range ms {
		m.Backoff(err)
	}
}

func successMail(ms []Mail) {
	for _, m := range ms {
		m.Success()
	}
}

// dialHost attempts to make a connection to the host specified by the Dialer.
// It returns MaxReconnectAttempts if the number of connection attempts has been
// exceeded.
func dialHost(ctx context.Context, dialer Dialer) (Sender, error) {
	sendAttempt := 0
	var sender Sender
	var err error
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		default:
			break
		}
		sender, err = dialer.Dial()
		if err == nil {
			break
		}
		sendAttempt++
		if sendAttempt == MaxReconnectAttempts {
			err = &ErrMaxConnectAttempts{
				underlyingError: err,
			}
			break
		}
	}
	return sender, err
}

// sendMail attempts to send the provided Mail instances.
// If the context is cancelled before all of the mail are sent,
// sendMail just returns and does not modify those emails.
func sendMail(ctx context.Context, dialer Dialer, job MailJob) {
	sender, err := dialHost(ctx, dialer)
	if err != nil {
		log.Warn(err)
		errorMail(err, job.Mails)
		return
	}
	defer sender.Close()

	errorMails := []Mail{}
	message := gomail.NewMessage()
	for i, m := range job.Mails {
		select {
		case <-ctx.Done():
			return
		default:
			break
		}
		message.Reset()
		err = m.Generate(message)
		if err != nil {
			log.Warn(err)
			m.Error(err)
			errorMails = append(errorMails, m)
			continue
		}

		// Filling the receiver header of message with recipient of mail logs when Bcc is enabled
		if job.Bcc {
			excludedMails := SetMessageRecipients(message, GetActiveMails(job, i, errorMails))
			errorMails = append(errorMails, excludedMails...)
		}

		activeMails := GetActiveMails(job, i, errorMails)

		err = gomail.Send(sender, message)
		if err != nil {
			if te, ok := err.(*textproto.Error); ok {
				switch {
				// If it's a temporary error, we should backoff and try again later.
				// We'll reset the connection so future messages don't incur a
				// different error (see https://github.com/gophish/gophish/issues/787).
				case te.Code >= 400 && te.Code <= 499:
					log.WithFields(logrus.Fields{
						"code":  te.Code,
						"email": GetRecipientDescription(message),
					}).Warn(err)
					backoffMail(err, activeMails)
					sender.Reset()
				// Otherwise, if it's a permanent error, we shouldn't backoff this message,
				// since the RFC specifies that running the same commands won't work next time.
				// We should reset our sender and error this message out.
				case te.Code >= 500 && te.Code <= 599:
					log.WithFields(logrus.Fields{
						"code":  te.Code,
						"email": GetRecipientDescription(message),
					}).Warn(err)
					errorMail(err, activeMails)
					sender.Reset()
				// If something else happened, let's just error out and reset the
				// sender
				default:
					log.WithFields(logrus.Fields{
						"code":  "unknown",
						"email": GetRecipientDescription(message),
					}).Warn(err)
					errorMail(err, activeMails)
					sender.Reset()
				}
			} else {
				// This likely indicates that something happened to the underlying
				// connection. We'll try to reconnect and, if that fails, we'll
				// error out the remaining emails.
				log.WithFields(logrus.Fields{
					"email": GetRecipientDescription(message),
				}).Warn(err)
				origErr := err
				sender, err = dialHost(ctx, dialer)
				if err != nil {
					if !job.Bcc {
						activeMails = job.Mails[i:]
					}
					errorMail(err, activeMails)
					break
				}
				backoffMail(origErr, activeMails)
			}
		} else {
			log.WithFields(logrus.Fields{
				"email": GetRecipientDescription(message),
			}).Info("Email sent")
			successMail(activeMails)
		}

		if job.Bcc {
			break
		}
	}
}

// GetActiveMails retrieve current mail that plan to be send.
// If the mail send over Bcc, returning the whole mail group
func GetActiveMails(job MailJob, index int, excludedMails []Mail) []Mail {
	mailEntries := []Mail{}
	if job.Bcc {
		for _, m := range job.Mails {
			excluded := false
			// Find the mail log which has error occur
			for _, excludedMail := range excludedMails {
				if m == excludedMail {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
			mailEntries = append(mailEntries, m)
		}
		return mailEntries
	}

	return []Mail{job.Mails[index]}
}

func SetMessageRecipients(msg *gomail.Message, ms []Mail) (excludedMails []Mail) {
	recipients := []string{}
	for _, m := range ms {
		recipient, err := m.GetRecipient()
		if err != nil {
			m.Error(err)
			excludedMails = append(excludedMails, m)
			continue
		}
		recipients = append(recipients, recipient)
	}
	// Remove To field from message header for Bcc
	msg.RemoveHeader("To")
	msg.SetHeader("Bcc", recipients...)
	return excludedMails
}

func GetRecipientDescription(message *gomail.Message) string {
	recipients := message.GetHeader("Bcc")
	recipientCount := len(recipients)

	switch recipientCount {
	case 0:
		return message.GetHeader("To")[0]
	case 1:
		return recipients[0]
	default:
		return fmt.Sprintf("%s and %d+ more", recipients[0], recipientCount)
	}
}
