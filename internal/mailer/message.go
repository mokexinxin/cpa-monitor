package mailer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

// Kind identifies whether a message announces a newly active alert or a
// recovery from one.
type Kind string

const (
	Alert    Kind = "ALERT"
	Recovery Kind = "RECOVERY"
)

// Event contains transport-independent alert content. Current, Threshold, and
// Details are strings so resource, network, health, and account alerts can all
// use the same message boundary.
type Event struct {
	Kind      Kind
	Object    string
	Hostname  string
	Timestamp time.Time
	Key       string
	Current   string
	Threshold string
	Details   string
	BaseURL   string
}

// BuildMessage constructs an RFC 5322 text message with CRLF line endings.
func BuildMessage(from string, to []string, event Event) ([]byte, error) {
	fromAddress, recipients, err := parseEnvelope(from, to)
	if err != nil {
		return nil, err
	}
	if err := validateEvent(event); err != nil {
		return nil, err
	}

	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("generate SMTP message ID: %w", err)
	}
	timestamp := event.Timestamp.UTC()
	subject := "[CPA Monitor] " + string(event.Kind) + " " + event.Object

	var message strings.Builder
	writeHeader(&message, "From", fromAddress.String())
	recipientHeaders := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		recipientHeaders = append(recipientHeaders, recipient.String())
	}
	writeHeader(&message, "To", strings.Join(recipientHeaders, ", "))
	writeHeader(&message, "Date", timestamp.Format(time.RFC1123Z))
	writeHeader(&message, "Message-ID", fmt.Sprintf("<%d.%s@cpa-monitor.local>", timestamp.UnixNano(), hex.EncodeToString(random)))
	writeHeader(&message, "Subject", mime.QEncoding.Encode("utf-8", subject))
	writeHeader(&message, "MIME-Version", "1.0")
	writeHeader(&message, "Content-Type", "text/plain; charset=UTF-8")
	writeHeader(&message, "Content-Transfer-Encoding", "quoted-printable")
	message.WriteString("\r\n")

	var body strings.Builder
	writeBodyField(&body, "Event", string(event.Kind))
	writeBodyField(&body, "Host", event.Hostname)
	writeBodyField(&body, "Timestamp", timestamp.Format(time.RFC3339))
	writeBodyField(&body, "Alert key", event.Key)
	writeBodyField(&body, "Current", event.Current)
	writeBodyField(&body, "Threshold", event.Threshold)
	body.WriteString("Details:\r\n")
	body.WriteString(normalizeCRLF(event.Details))
	body.WriteString("\r\n")
	writeBodyField(&body, "CLIProxyAPI base URL", event.BaseURL)

	encoder := quotedprintable.NewWriter(&message)
	if _, err := encoder.Write([]byte(body.String())); err != nil {
		return nil, fmt.Errorf("encode SMTP message body: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("finish SMTP message body: %w", err)
	}

	return []byte(message.String()), nil
}

func validateEvent(event Event) error {
	if event.Kind != Alert && event.Kind != Recovery {
		return fmt.Errorf("mail event kind must be ALERT or RECOVERY")
	}
	if strings.TrimSpace(event.Object) == "" {
		return fmt.Errorf("mail event object must not be empty")
	}
	if containsHeaderControl(event.Object) {
		return fmt.Errorf("mail event object must not contain header control characters")
	}
	if strings.TrimSpace(event.Hostname) == "" {
		return fmt.Errorf("mail event hostname must not be empty")
	}
	if event.Timestamp.IsZero() {
		return fmt.Errorf("mail event timestamp must not be zero")
	}
	if strings.TrimSpace(event.Key) == "" {
		return fmt.Errorf("mail event key must not be empty")
	}
	if strings.TrimSpace(event.BaseURL) == "" {
		return fmt.Errorf("mail event base URL must not be empty")
	}
	return nil
}

func parseEnvelope(from string, to []string) (*mail.Address, []*mail.Address, error) {
	if containsLineBreak(from) {
		return nil, nil, fmt.Errorf("SMTP From address must not contain line breaks")
	}
	fromAddress, err := mail.ParseAddress(from)
	if err != nil || !validMailboxAddress(fromAddress) {
		return nil, nil, fmt.Errorf("SMTP From address is invalid")
	}
	if len(to) == 0 {
		return nil, nil, fmt.Errorf("SMTP recipient list must not be empty")
	}
	recipients := make([]*mail.Address, 0, len(to))
	for _, value := range to {
		if containsLineBreak(value) {
			return nil, nil, fmt.Errorf("SMTP recipient address must not contain line breaks")
		}
		recipient, err := mail.ParseAddress(value)
		if err != nil || !validMailboxAddress(recipient) {
			return nil, nil, fmt.Errorf("SMTP recipient address is invalid")
		}
		recipients = append(recipients, recipient)
	}
	return fromAddress, recipients, nil
}

func validMailboxAddress(address *mail.Address) bool {
	if address == nil {
		return false
	}
	at := strings.LastIndexByte(address.Address, '@')
	return at > 0 && at < len(address.Address)-1
}

func writeHeader(builder *strings.Builder, name, value string) {
	builder.WriteString(name)
	builder.WriteString(": ")
	builder.WriteString(value)
	builder.WriteString("\r\n")
}

func writeBodyField(builder *strings.Builder, name, value string) {
	builder.WriteString(name)
	builder.WriteString(": ")
	builder.WriteString(normalizeCRLF(value))
	builder.WriteString("\r\n")
}

func normalizeCRLF(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.ReplaceAll(value, "\n", "\r\n")
}

func containsLineBreak(value string) bool {
	return strings.ContainsAny(value, "\r\n")
}

func containsHeaderControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}
