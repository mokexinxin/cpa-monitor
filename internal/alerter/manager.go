// Package alerter reconciles current rule conditions with persisted active
// alert state. State advances only after the corresponding email succeeds.
package alerter

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mokexinxin/cpa-monitor/internal/mailer"
	"github.com/mokexinxin/cpa-monitor/internal/rule"
	"github.com/mokexinxin/cpa-monitor/internal/state"
)

// Sender is the narrow mail transport boundary used by Manager.
type Sender interface {
	Send(context.Context, mailer.Event) error
}

// Store is the narrow active-alert state boundary used by Manager.
type Store interface {
	ByScope(scope string) []state.Record
	Put(state.Record) error
	Delete(key string) bool
	Save() error
}

var _ Store = (*state.File)(nil)

// Manager reconciles one scope at a time. Its mutex makes the send/mutate/save
// transaction serial even if callers invoke Reconcile concurrently.
type Manager struct {
	mu           sync.Mutex
	sender       Sender
	store        Store
	hostname     string
	baseURL      string
	sendRecovery bool
	now          func() time.Time
	dirty        bool
}

func NewManager(sender Sender, store Store, hostname, baseURL string, sendRecovery bool) *Manager {
	return &Manager{
		sender:       sender,
		store:        store,
		hostname:     hostname,
		baseURL:      baseURL,
		sendRecovery: sendRecovery,
		now:          time.Now,
	}
}

// Reconcile sends new alerts and, for a complete batch only, recovers active
// keys missing from the batch. All operations are processed by key. A failed
// operation does not prevent later keys from being attempted. If any in-memory
// mutation succeeds, Save is attempted once after all operations. A failed
// Save stays dirty and is retried at most once by the next valid batch without
// resending already-active alerts.
func (m *Manager) Reconcile(ctx context.Context, batch rule.Batch) error {
	if m == nil {
		return errors.New("reconcile alerts: nil manager")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.validate(ctx, batch); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	conditions := make(map[string]rule.Condition, len(batch.Conditions))
	for _, condition := range batch.Conditions {
		conditions[condition.Key] = condition
	}

	active := m.store.ByScope(batch.Scope)
	activeByKey := make(map[string]state.Record, len(active))
	for _, record := range active {
		activeByKey[record.Key] = record
	}

	operations := make([]operation, 0, len(conditions)+len(activeByKey))
	for key, condition := range conditions {
		if _, alreadyActive := activeByKey[key]; alreadyActive {
			continue
		}
		operations = append(operations, operation{
			key:       key,
			kind:      mailer.Alert,
			condition: condition,
		})
	}
	if batch.Complete {
		for key, record := range activeByKey {
			if _, stillUnhealthy := conditions[key]; stillUnhealthy {
				continue
			}
			operations = append(operations, operation{
				key:    key,
				kind:   mailer.Recovery,
				record: record,
			})
		}
	}
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].key == operations[j].key {
			return operations[i].kind < operations[j].kind
		}
		return operations[i].key < operations[j].key
	})

	var reconcileErrors []error
	mutated := false
	for _, operation := range operations {
		if err := ctx.Err(); err != nil {
			reconcileErrors = append(reconcileErrors, err)
			break
		}

		switch operation.kind {
		case mailer.Alert:
			timestamp := m.timestamp()
			if err := m.sender.Send(ctx, m.alertEvent(operation.condition, timestamp)); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("send alert %q: %w", operation.key, err))
				continue
			}
			if err := m.store.Put(recordFromCondition(operation.condition, timestamp)); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("activate alert %q: %w", operation.key, err))
				continue
			}
			mutated = true

		case mailer.Recovery:
			if m.sendRecovery {
				if err := m.sender.Send(ctx, m.recoveryEvent(operation.record, m.timestamp())); err != nil {
					reconcileErrors = append(reconcileErrors, fmt.Errorf("send recovery %q: %w", operation.key, err))
					continue
				}
			}
			if m.store.Delete(operation.key) {
				mutated = true
			}
		}
	}

	if mutated {
		m.dirty = true
	}
	if m.dirty {
		if err := m.store.Save(); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("save alert state: %w", err))
		} else {
			m.dirty = false
		}
	}
	return errors.Join(reconcileErrors...)
}

type operation struct {
	key       string
	kind      mailer.Kind
	condition rule.Condition
	record    state.Record
}

func (m *Manager) validate(ctx context.Context, batch rule.Batch) error {
	if ctx == nil {
		return errors.New("reconcile alerts: nil context")
	}
	if m.sender == nil {
		return errors.New("reconcile alerts: nil sender")
	}
	if m.store == nil {
		return errors.New("reconcile alerts: nil store")
	}
	if strings.TrimSpace(m.hostname) == "" {
		return errors.New("reconcile alerts: hostname is required")
	}
	if strings.TrimSpace(m.baseURL) == "" {
		return errors.New("reconcile alerts: base URL is required")
	}
	if strings.TrimSpace(batch.Scope) == "" {
		return errors.New("reconcile alerts: batch scope is required")
	}
	seen := make(map[string]struct{}, len(batch.Conditions))
	for i, condition := range batch.Conditions {
		if strings.TrimSpace(condition.Key) == "" {
			return fmt.Errorf("reconcile alerts: condition %d key is required", i+1)
		}
		if condition.Scope != batch.Scope {
			return fmt.Errorf("reconcile alerts: condition %q scope %q does not match batch scope %q", condition.Key, condition.Scope, batch.Scope)
		}
		if _, duplicate := seen[condition.Key]; duplicate {
			return fmt.Errorf("reconcile alerts: duplicate condition key %q", condition.Key)
		}
		seen[condition.Key] = struct{}{}
	}
	return nil
}

func (m *Manager) timestamp() time.Time {
	now := m.now
	if now == nil {
		now = time.Now
	}
	return now().UTC()
}

func (m *Manager) alertEvent(condition rule.Condition, timestamp time.Time) mailer.Event {
	return mailer.Event{
		Kind:      mailer.Alert,
		Object:    objectName(condition.Summary, condition.Key),
		Hostname:  m.hostname,
		Timestamp: timestamp,
		Key:       condition.Key,
		Current:   condition.Current,
		Threshold: condition.Threshold,
		Details:   formatDetails(condition.Details),
		BaseURL:   m.baseURL,
	}
}

func (m *Manager) recoveryEvent(record state.Record, timestamp time.Time) mailer.Event {
	return mailer.Event{
		Kind:      mailer.Recovery,
		Object:    objectName(record.Summary, record.Key) + " recovered",
		Hostname:  m.hostname,
		Timestamp: timestamp,
		Key:       record.Key,
		Current:   "recovered",
		Threshold: record.Threshold,
		Details:   formatDetails(record.Details),
		BaseURL:   m.baseURL,
	}
}

func objectName(summary, key string) string {
	value := key
	if strings.TrimSpace(summary) != "" {
		value = summary
	}
	return headerSafe(value)
}

func headerSafe(value string) string {
	var safe strings.Builder
	for _, character := range value {
		switch character {
		case '\r':
			safe.WriteString(`\r`)
		case '\n':
			safe.WriteString(`\n`)
		case '\t':
			safe.WriteString(`\t`)
		default:
			if character < 0x20 || character == 0x7f {
				fmt.Fprintf(&safe, `\u%04X`, character)
			} else {
				safe.WriteRune(character)
			}
		}
	}
	return safe.String()
}

func recordFromCondition(condition rule.Condition, activatedAt time.Time) state.Record {
	return state.Record{
		Key:         condition.Key,
		Scope:       condition.Scope,
		Summary:     condition.Summary,
		Current:     condition.Current,
		Threshold:   condition.Threshold,
		Details:     cloneDetails(condition.Details),
		ActivatedAt: activatedAt.UTC(),
	}
}

func cloneDetails(details map[string]string) map[string]string {
	if details == nil {
		return nil
	}
	cloned := make(map[string]string, len(details))
	for key, value := range details {
		cloned[key] = value
	}
	return cloned
}

func formatDetails(details map[string]string) string {
	if len(details) == 0 {
		return ""
	}
	keys := make([]string, 0, len(details))
	for key := range details {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+"="+details[key])
	}
	return strings.Join(lines, "\n")
}
