package mailer

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testSMTPUsername = "smtp-user-must-stay-secret"
	testSMTPPassword = "smtp-password-must-stay-secret"
)

func TestNewValidatesSMTPConfig(t *testing.T) {
	t.Parallel()

	valid := Config{
		Host:      "smtp.example.test",
		Port:      587,
		From:      "monitor@example.test",
		To:        []string{"admin@example.test"},
		StartTLS:  true,
		Timeout:   time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "empty host", mutate: func(c *Config) { c.Host = "" }},
		{name: "host with port", mutate: func(c *Config) { c.Host = "smtp.example.test:587" }},
		{name: "zero port", mutate: func(c *Config) { c.Port = 0 }},
		{name: "large port", mutate: func(c *Config) { c.Port = 65536 }},
		{name: "invalid from", mutate: func(c *Config) { c.From = "not-an-address" }},
		{name: "from injection", mutate: func(c *Config) { c.From = "monitor@example.test\r\nBcc: stolen@example.test" }},
		{name: "no recipients", mutate: func(c *Config) { c.To = nil }},
		{name: "invalid recipient", mutate: func(c *Config) { c.To = []string{"not-an-address"} }},
		{name: "recipient injection", mutate: func(c *Config) { c.To = []string{"admin@example.test\nBcc: stolen@example.test"} }},
		{name: "username only", mutate: func(c *Config) { c.Username = testSMTPUsername }},
		{name: "password only", mutate: func(c *Config) { c.Password = testSMTPPassword }},
		{name: "username injection", mutate: func(c *Config) {
			c.Username = testSMTPUsername + "\r\n"
			c.Password = testSMTPPassword
		}},
		{name: "plaintext", mutate: func(c *Config) { c.StartTLS = false }},
		{name: "both TLS modes", mutate: func(c *Config) { c.DirectTLS = true }},
		{name: "zero timeout", mutate: func(c *Config) { c.Timeout = 0 }},
		{name: "invalid language", mutate: func(c *Config) { c.Language = "fr" }},
		{name: "insecure TLS", mutate: func(c *Config) {
			c.TLSConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec -- verifies rejection.
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			cfg.To = append([]string(nil), valid.To...)
			tt.mutate(&cfg)
			_, err := New(cfg)
			if err == nil {
				t.Fatal("New() error = nil, want validation error")
			}
			if strings.Contains(err.Error(), testSMTPUsername) || strings.Contains(err.Error(), testSMTPPassword) {
				t.Fatalf("New() error leaked SMTP credentials: %v", err)
			}
		})
	}
}

func TestNewDefaultsLanguageToChinese(t *testing.T) {
	t.Parallel()
	config := Config{
		Host: "smtp.example.test", Port: 587, From: "monitor@example.test",
		To: []string{"admin@example.test"}, StartTLS: true, Timeout: time.Second,
	}
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if client.config.Language != LanguageChinese {
		t.Fatalf("language = %q, want %q", client.config.Language, LanguageChinese)
	}
}

func TestSendDeliversOverVerifiedTLS(t *testing.T) {
	for _, tt := range []struct {
		name      string
		directTLS bool
		auth      bool
	}{
		{name: "STARTTLS without auth"},
		{name: "STARTTLS with auth", auth: true},
		{name: "direct TLS with auth", directTLS: true, auth: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newFakeSMTPServer(t, fakeSMTPOptions{directTLS: tt.directTLS})
			defer server.closeAndCheck(t)

			cfg := server.clientConfig()
			cfg.Language = LanguageEnglish
			cfg.From = "CPA Monitor <monitor@example.test>"
			cfg.To = []string{"one@example.test", "Admin Two <two@example.test>"}
			if tt.auth {
				cfg.Username = testSMTPUsername
				cfg.Password = testSMTPPassword
			}
			client, err := New(cfg)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if err := client.Send(context.Background(), validEvent()); err != nil {
				t.Fatalf("Send() error = %v", err)
			}

			snapshot := server.snapshot()
			if snapshot.sni != "localhost" {
				t.Errorf("TLS SNI = %q, want localhost", snapshot.sni)
			}
			if got, want := snapshot.recipients, []string{"one@example.test", "two@example.test"}; !equalStrings(got, want) {
				t.Errorf("recipients = %#v, want %#v", got, want)
			}
			if !snapshot.mailWasSecure || !snapshot.dataWasSecure {
				t.Errorf("message commands were not protected by TLS: %#v", snapshot)
			}
			if tt.directTLS && snapshot.sawStartTLS {
				t.Error("direct TLS connection sent STARTTLS")
			}
			if !tt.directTLS && !snapshot.sawStartTLS {
				t.Error("STARTTLS connection did not send STARTTLS")
			}
			if tt.auth {
				wantAuth := "\x00" + testSMTPUsername + "\x00" + testSMTPPassword
				if snapshot.authPayload != wantAuth || !snapshot.authWasSecure {
					t.Errorf("AUTH payload/transport = %q secure=%v, want expected credentials over TLS", snapshot.authPayload, snapshot.authWasSecure)
				}
			} else if snapshot.authPayload != "" {
				t.Errorf("unexpected AUTH payload %q", snapshot.authPayload)
			}
			if !snapshot.sawQuit {
				t.Error("client did not issue QUIT")
			}

			message, err := mail.ReadMessage(strings.NewReader(snapshot.data))
			if err != nil {
				t.Fatalf("delivered data is not an RFC message: %v\n%s", err, snapshot.data)
			}
			if got := message.Header.Get("Subject"); got != "[CPA Monitor] ALERT memory usage 84.2% on host" {
				t.Errorf("delivered Subject = %q", got)
			}
		})
	}
}

func TestSendHealthDeliversMultipartReport(t *testing.T) {
	server := newFakeSMTPServer(t, fakeSMTPOptions{})
	defer server.closeAndCheck(t)
	config := server.clientConfig()
	config.Language = LanguageEnglish
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendHealth(context.Background(), validHealthReport()); err != nil {
		t.Fatalf("SendHealth() error = %v", err)
	}
	message, err := mail.ReadMessage(strings.NewReader(server.snapshot().data))
	if err != nil {
		t.Fatal(err)
	}
	if got := message.Header.Get("Subject"); got != "[CPA Monitor] SERVER STATUS monitor-01" {
		t.Fatalf("Subject = %q", got)
	}
	parts := readAlternative(t, message)
	if !strings.Contains(string(parts["text/html"]), "Server systems are operating normally") {
		t.Fatal("delivered HTML health report is missing status heading")
	}
}

func TestSendRejectsUntrustedOrWrongNameCertificates(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		check  func(error) bool
	}{
		{
			name: "untrusted certificate",
			mutate: func(c *Config) {
				c.TLSConfig = nil
			},
			check: func(err error) bool {
				var verification *tls.CertificateVerificationError
				return errors.As(err, &verification)
			},
		},
		{
			name: "wrong server name",
			mutate: func(c *Config) {
				c.TLSConfig.ServerName = "wrong.example.test"
			},
			check: func(err error) bool {
				var verification *tls.CertificateVerificationError
				return errors.As(err, &verification)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newFakeSMTPServer(t, fakeSMTPOptions{directTLS: true})
			defer server.closeAndCheck(t)
			cfg := server.clientConfig()
			tt.mutate(&cfg)

			client, err := New(cfg)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			err = client.Send(context.Background(), validEvent())
			if err == nil {
				t.Fatal("Send() error = nil, want TLS verification failure")
			}
			if !tt.check(err) {
				t.Fatalf("Send() error = %T %v, want certificate verification error", err, err)
			}
		})
	}
}

func TestSendHonorsContextCancellationAndTimeout(t *testing.T) {
	t.Run("cancellation during greeting", func(t *testing.T) {
		server := newFakeSMTPServer(t, fakeSMTPOptions{stallGreeting: true})
		defer server.closeAndCheck(t)
		cfg := server.clientConfig()
		cfg.Timeout = 5 * time.Second
		client, err := New(cfg)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- client.Send(ctx, validEvent()) }()
		select {
		case <-server.accepted:
		case <-time.After(time.Second):
			t.Fatal("SMTP server did not accept connection")
		}
		cancel()

		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Send() error = %v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Send() did not stop promptly after cancellation")
		}
	})

	t.Run("configured timeout", func(t *testing.T) {
		server := newFakeSMTPServer(t, fakeSMTPOptions{stallGreeting: true})
		defer server.closeAndCheck(t)
		cfg := server.clientConfig()
		cfg.Timeout = 30 * time.Millisecond
		client, err := New(cfg)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}

		err = client.Send(context.Background(), validEvent())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Send() error = %v, want context.DeadlineExceeded", err)
		}
	})
}

func TestSendReturnsProtocolStageErrors(t *testing.T) {
	for _, stage := range []string{"greeting", "starttls", "auth", "mail", "rcpt", "data", "data-end", "quit"} {
		stage := stage
		t.Run(stage, func(t *testing.T) {
			server := newFakeSMTPServer(t, fakeSMTPOptions{failAt: stage})
			defer server.closeAndCheck(t)
			cfg := server.clientConfig()
			cfg.Username = testSMTPUsername
			cfg.Password = testSMTPPassword
			client, err := New(cfg)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			err = client.Send(context.Background(), validEvent())
			if err == nil {
				t.Fatalf("Send() error = nil, want %s failure", stage)
			}
			if strings.Contains(err.Error(), testSMTPUsername) || strings.Contains(err.Error(), testSMTPPassword) {
				t.Fatalf("Send() error leaked SMTP credentials: %v", err)
			}
		})
	}
}

type fakeSMTPOptions struct {
	directTLS     bool
	stallGreeting bool
	failAt        string
}

type fakeSMTPSnapshot struct {
	commands      []string
	recipients    []string
	data          string
	sni           string
	authPayload   string
	authWasSecure bool
	mailWasSecure bool
	dataWasSecure bool
	sawStartTLS   bool
	sawQuit       bool
}

type fakeSMTPServer struct {
	listener net.Listener
	options  fakeSMTPOptions
	cert     tls.Certificate
	roots    *x509.CertPool
	accepted chan struct{}
	done     chan struct{}

	mu            sync.Mutex
	snapshotValue fakeSMTPSnapshot
	serveErr      error
}

func newFakeSMTPServer(t *testing.T, options fakeSMTPOptions) *fakeSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cert, roots := makeTestCertificate(t)
	server := &fakeSMTPServer{
		listener: listener,
		options:  options,
		cert:     cert,
		roots:    roots,
		accepted: make(chan struct{}),
		done:     make(chan struct{}),
	}
	go server.serve()
	return server
}

func (s *fakeSMTPServer) clientConfig() Config {
	port := s.listener.Addr().(*net.TCPAddr).Port
	return Config{
		Host:      "localhost",
		Port:      port,
		From:      "monitor@example.test",
		To:        []string{"admin@example.test"},
		StartTLS:  !s.options.directTLS,
		DirectTLS: s.options.directTLS,
		Timeout:   2 * time.Second,
		TLSConfig: &tls.Config{RootCAs: s.roots, MinVersion: tls.VersionTLS12},
	}
}

func (s *fakeSMTPServer) serve() {
	defer close(s.done)
	raw, err := s.listener.Accept()
	if err != nil {
		if !errors.Is(err, net.ErrClosed) {
			s.setServeError(err)
		}
		return
	}
	close(s.accepted)
	defer raw.Close()

	if s.options.stallGreeting {
		_, _ = io.Copy(io.Discard, raw)
		return
	}

	conn := raw
	secure := false
	if s.options.directTLS {
		conn, err = s.wrapTLS(raw)
		if err != nil {
			// Client-side certificate failures make a corresponding server-side
			// handshake error expected in verification tests.
			return
		}
		secure = true
	}

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	if s.options.failAt == "greeting" {
		if err := writeSMTPLine(writer, "554 greeting rejected"); err != nil {
			s.setServeError(err)
		}
		return
	}
	if err := writeSMTPLine(writer, "220 fake SMTP ready"); err != nil {
		s.setServeError(err)
		return
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) && !isClosedError(err) {
				s.setServeError(err)
			}
			return
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		s.recordCommand(line)
		command, argument, _ := strings.Cut(line, " ")
		switch strings.ToUpper(command) {
		case "EHLO":
			if !secure {
				if err := writeSMTPMultiline(writer, []string{"fake", "STARTTLS"}); err != nil {
					s.setServeError(err)
					return
				}
			} else if err := writeSMTPMultiline(writer, []string{"fake", "AUTH PLAIN"}); err != nil {
				s.setServeError(err)
				return
			}
		case "HELO":
			if err := writeSMTPLine(writer, "250 fake"); err != nil {
				s.setServeError(err)
				return
			}
		case "STARTTLS":
			s.updateSnapshot(func(snapshot *fakeSMTPSnapshot) { snapshot.sawStartTLS = true })
			if s.options.failAt == "starttls" {
				_ = writeSMTPLine(writer, "454 TLS temporarily unavailable")
				return
			}
			if err := writeSMTPLine(writer, "220 begin TLS"); err != nil {
				s.setServeError(err)
				return
			}
			conn, err = s.wrapTLS(raw)
			if err != nil {
				return
			}
			secure = true
			reader = bufio.NewReader(conn)
			writer = bufio.NewWriter(conn)
		case "AUTH":
			parts := strings.Fields(argument)
			payload := ""
			if len(parts) == 2 && strings.EqualFold(parts[0], "PLAIN") {
				decoded, decodeErr := base64.StdEncoding.DecodeString(parts[1])
				if decodeErr == nil {
					payload = string(decoded)
				}
			}
			s.updateSnapshot(func(snapshot *fakeSMTPSnapshot) {
				snapshot.authPayload = payload
				snapshot.authWasSecure = secure
			})
			if s.options.failAt == "auth" {
				_ = writeSMTPLine(writer, "535 rejected "+testSMTPUsername+" "+testSMTPPassword)
				return
			}
			if err := writeSMTPLine(writer, "235 authentication successful"); err != nil {
				s.setServeError(err)
				return
			}
		case "MAIL":
			s.updateSnapshot(func(snapshot *fakeSMTPSnapshot) { snapshot.mailWasSecure = secure })
			if s.options.failAt == "mail" {
				_ = writeSMTPLine(writer, "550 sender rejected")
				return
			}
			if err := writeSMTPLine(writer, "250 sender accepted"); err != nil {
				s.setServeError(err)
				return
			}
		case "RCPT":
			if s.options.failAt == "rcpt" {
				_ = writeSMTPLine(writer, "550 recipient rejected")
				return
			}
			recipient := smtpPath(argument)
			s.updateSnapshot(func(snapshot *fakeSMTPSnapshot) {
				snapshot.recipients = append(snapshot.recipients, recipient)
			})
			if err := writeSMTPLine(writer, "250 recipient accepted"); err != nil {
				s.setServeError(err)
				return
			}
		case "DATA":
			if s.options.failAt == "data" {
				_ = writeSMTPLine(writer, "554 data rejected")
				return
			}
			if err := writeSMTPLine(writer, "354 end with dot"); err != nil {
				s.setServeError(err)
				return
			}
			var data strings.Builder
			for {
				dataLine, readErr := reader.ReadString('\n')
				if readErr != nil {
					s.setServeError(readErr)
					return
				}
				if dataLine == ".\r\n" || dataLine == ".\n" {
					break
				}
				if strings.HasPrefix(dataLine, "..") {
					dataLine = dataLine[1:]
				}
				data.WriteString(dataLine)
			}
			s.updateSnapshot(func(snapshot *fakeSMTPSnapshot) {
				snapshot.data = data.String()
				snapshot.dataWasSecure = secure
			})
			if s.options.failAt == "data-end" {
				_ = writeSMTPLine(writer, "554 message rejected")
				return
			}
			if err := writeSMTPLine(writer, "250 queued"); err != nil {
				s.setServeError(err)
				return
			}
		case "QUIT":
			s.updateSnapshot(func(snapshot *fakeSMTPSnapshot) { snapshot.sawQuit = true })
			if s.options.failAt == "quit" {
				_ = writeSMTPLine(writer, "554 quit rejected")
				return
			}
			if err := writeSMTPLine(writer, "221 bye"); err != nil {
				s.setServeError(err)
			}
			return
		default:
			_ = writeSMTPLine(writer, "500 unknown command")
			return
		}
	}
}

func (s *fakeSMTPServer) wrapTLS(conn net.Conn) (net.Conn, error) {
	config := &tls.Config{
		Certificates: []tls.Certificate{s.cert},
		MinVersion:   tls.VersionTLS12,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			s.updateSnapshot(func(snapshot *fakeSMTPSnapshot) { snapshot.sni = hello.ServerName })
			return nil, nil
		},
	}
	tlsConn := tls.Server(conn, config)
	if err := tlsConn.Handshake(); err != nil {
		return nil, err
	}
	return tlsConn, nil
}

func (s *fakeSMTPServer) closeAndCheck(t *testing.T) {
	t.Helper()
	_ = s.listener.Close()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Error("fake SMTP server did not stop")
		return
	}
	s.mu.Lock()
	err := s.serveErr
	s.mu.Unlock()
	if err != nil {
		t.Errorf("fake SMTP server error: %v", err)
	}
}

func (s *fakeSMTPServer) snapshot() fakeSMTPSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := s.snapshotValue
	copy.commands = append([]string(nil), copy.commands...)
	copy.recipients = append([]string(nil), copy.recipients...)
	return copy
}

func (s *fakeSMTPServer) updateSnapshot(update func(*fakeSMTPSnapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	update(&s.snapshotValue)
}

func (s *fakeSMTPServer) recordCommand(line string) {
	s.updateSnapshot(func(snapshot *fakeSMTPSnapshot) {
		snapshot.commands = append(snapshot.commands, line)
	})
}

func (s *fakeSMTPServer) setServeError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serveErr == nil {
		s.serveErr = err
	}
}

func makeTestCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "CPA Monitor Test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatalf("parse server key pair: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	return cert, roots
}

func writeSMTPLine(writer *bufio.Writer, line string) error {
	if _, err := writer.WriteString(line + "\r\n"); err != nil {
		return err
	}
	return writer.Flush()
}

func writeSMTPMultiline(writer *bufio.Writer, lines []string) error {
	for i, line := range lines {
		separator := "-"
		if i == len(lines)-1 {
			separator = " "
		}
		if _, err := fmt.Fprintf(writer, "250%s%s\r\n", separator, line); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func smtpPath(argument string) string {
	_, path, ok := strings.Cut(argument, ":")
	if !ok {
		return ""
	}
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "<")
	path = strings.TrimSuffix(path, ">")
	return path
}

func isClosedError(err error) bool {
	return errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection")
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
