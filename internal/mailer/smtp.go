package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Config controls SMTP delivery. Exactly one encrypted transport mode must be
// enabled. TLSConfig is optional; when supplied it is cloned, certificate
// verification must remain enabled, and an empty ServerName defaults to Host.
type Config struct {
	Host      string
	Port      int
	Language  string
	Username  string
	Password  string
	From      string
	To        []string
	StartTLS  bool
	DirectTLS bool
	Timeout   time.Duration
	TLSConfig *tls.Config
}

// Mailer sends alert Events through a validated SMTP configuration.
type Mailer struct {
	config      Config
	tlsConfig   *tls.Config
	fromAddress string
	recipients  []string
	dialContext func(context.Context, string, string) (net.Conn, error)
}

// New validates and copies a mailer configuration. Plaintext SMTP is not
// supported, including when authentication is disabled.
func New(config Config) (*Mailer, error) {
	if config.Language == "" {
		config.Language = "zh-CN"
	}
	if !validSMTPHost(config.Host) {
		return nil, fmt.Errorf("SMTP host is invalid")
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, fmt.Errorf("SMTP port must be between 1 and 65535")
	}
	from, to, err := parseEnvelope(config.From, config.To)
	if err != nil {
		return nil, err
	}
	if (config.Username == "") != (config.Password == "") {
		return nil, fmt.Errorf("SMTP username and password must both be set or both be empty")
	}
	if containsCredentialControl(config.Username) || containsCredentialControl(config.Password) {
		return nil, fmt.Errorf("SMTP credentials contain invalid characters")
	}
	if config.StartTLS == config.DirectTLS {
		return nil, fmt.Errorf("exactly one of SMTP STARTTLS and direct TLS must be enabled")
	}
	if config.Timeout <= 0 {
		return nil, fmt.Errorf("SMTP timeout must be greater than zero")
	}
	if !validLanguage(config.Language) {
		return nil, fmt.Errorf("SMTP language must be zh-CN or en")
	}

	tlsConfig := &tls.Config{}
	if config.TLSConfig != nil {
		if config.TLSConfig.InsecureSkipVerify {
			return nil, fmt.Errorf("SMTP TLS certificate verification cannot be disabled")
		}
		tlsConfig = config.TLSConfig.Clone()
	}
	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = config.Host
	}
	if tlsConfig.MinVersion == 0 {
		tlsConfig.MinVersion = tls.VersionTLS12
	}

	recipients := make([]string, 0, len(to))
	for _, recipient := range to {
		recipients = append(recipients, recipient.Address)
	}
	config.To = append([]string(nil), config.To...)
	config.TLSConfig = nil
	dialer := &net.Dialer{}
	return &Mailer{
		config:      config,
		tlsConfig:   tlsConfig,
		fromAddress: from.Address,
		recipients:  recipients,
		dialContext: dialer.DialContext,
	}, nil
}

// Send constructs and delivers one alert or recovery event.
func (m *Mailer) Send(ctx context.Context, event Event) error {
	if ctx == nil {
		return fmt.Errorf("SMTP context must not be nil")
	}
	message, err := BuildMessageInLanguage(m.config.From, m.config.To, event, m.config.Language)
	if err != nil {
		return err
	}
	return m.send(ctx, message)
}

// SendHealth constructs and delivers one scheduled healthy-status report.
func (m *Mailer) SendHealth(ctx context.Context, report HealthReport) error {
	if ctx == nil {
		return fmt.Errorf("SMTP context must not be nil")
	}
	message, err := BuildHealthMessageInLanguage(m.config.From, m.config.To, report, m.config.Language)
	if err != nil {
		return err
	}
	return m.send(ctx, message)
}

func (m *Mailer) send(ctx context.Context, message []byte) error {
	if err := ctx.Err(); err != nil {
		return m.sendError(ctx, "SMTP send canceled", err)
	}

	sendCtx, cancel := context.WithTimeout(ctx, m.config.Timeout)
	defer cancel()
	address := net.JoinHostPort(m.config.Host, strconv.Itoa(m.config.Port))
	conn, err := m.dialContext(sendCtx, "tcp", address)
	if err != nil {
		return m.sendError(sendCtx, "connect to SMTP server", err)
	}
	defer conn.Close()
	if deadline, ok := sendCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return m.sendError(sendCtx, "set SMTP connection deadline", err)
		}
	}
	stopCancellationWatch := watchConnectionCancellation(sendCtx, conn)
	defer stopCancellationWatch()

	var client *smtp.Client
	if m.config.DirectTLS {
		tlsConn := tls.Client(conn, m.tlsConfig.Clone())
		if err := tlsConn.HandshakeContext(sendCtx); err != nil {
			return m.sendError(sendCtx, "establish direct SMTP TLS", err)
		}
		client, err = smtp.NewClient(tlsConn, m.config.Host)
		if err != nil {
			return m.sendError(sendCtx, "read SMTP greeting", err)
		}
	} else {
		client, err = smtp.NewClient(conn, m.config.Host)
		if err != nil {
			return m.sendError(sendCtx, "read SMTP greeting", err)
		}
		if err := client.StartTLS(m.tlsConfig.Clone()); err != nil {
			_ = client.Close()
			return m.sendError(sendCtx, "start SMTP TLS", err)
		}
	}
	defer client.Close()

	if m.config.Username != "" {
		auth := smtp.PlainAuth("", m.config.Username, m.config.Password, m.config.Host)
		if err := client.Auth(auth); err != nil {
			return m.sendError(sendCtx, "authenticate to SMTP server", err)
		}
	}
	if err := client.Mail(m.fromAddress); err != nil {
		return m.sendError(sendCtx, "send SMTP MAIL command", err)
	}
	for _, recipient := range m.recipients {
		if err := client.Rcpt(recipient); err != nil {
			return m.sendError(sendCtx, "send SMTP RCPT command", err)
		}
	}

	data, err := client.Data()
	if err != nil {
		return m.sendError(sendCtx, "start SMTP message data", err)
	}
	if _, err := data.Write(message); err != nil {
		_ = data.Close()
		return m.sendError(sendCtx, "write SMTP message data", err)
	}
	if err := data.Close(); err != nil {
		return m.sendError(sendCtx, "finish SMTP message data", err)
	}
	if err := client.Quit(); err != nil {
		return m.sendError(sendCtx, "quit SMTP session", err)
	}
	return nil
}

func (m *Mailer) sendError(ctx context.Context, stage string, cause error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		cause = ctxErr
	} else {
		var timeout net.Error
		if errors.As(cause, &timeout) && timeout.Timeout() {
			cause = context.DeadlineExceeded
		}
	}
	detail := cause.Error()
	for _, secret := range []string{m.config.Username, m.config.Password} {
		if secret != "" {
			detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
		}
	}
	return &smtpError{
		message: stage + ": " + detail,
		cause:   cause,
	}
}

type smtpError struct {
	message string
	cause   error
}

func (e *smtpError) Error() string { return e.message }
func (e *smtpError) Unwrap() error { return e.cause }

func watchConnectionCancellation(ctx context.Context, conn net.Conn) func() {
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-stopped:
		}
	}()
	return func() { close(stopped) }
}

func validSMTPHost(host string) bool {
	if host == "" || host != strings.TrimSpace(host) || len(host) > 253 {
		return false
	}
	for _, character := range host {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return false
		}
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if strings.ContainsAny(host, ":/[]") {
		return false
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

func containsCredentialControl(value string) bool {
	for _, character := range value {
		if character == '\x00' || character == '\r' || character == '\n' {
			return true
		}
	}
	return false
}
