package alerter

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mokexinxin/cpa-monitor/internal/mailer"
	"github.com/mokexinxin/cpa-monitor/internal/rule"
	"github.com/mokexinxin/cpa-monitor/internal/state"
)

func TestManagerNewAlertsAreSentActivatedSortedAndDeduplicated(t *testing.T) {
	t.Parallel()

	sender := newFakeSender()
	store := newFakeStore()
	manager := newTestManager(sender, store, false)
	batch := rule.Batch{
		Scope:    rule.ScopeDisk,
		Complete: true,
		Conditions: []rule.Condition{
			condition("resource:disk:/z", rule.ScopeDisk, "disk /z usage 90%"),
			condition("resource:disk:/a", rule.ScopeDisk, "disk /a usage 85%"),
		},
	}
	batch.Conditions[1].Details = map[string]string{"used": "85", "filesystem": "ext4"}

	if err := manager.Reconcile(context.Background(), batch); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	events := sender.Events()
	assertEventKeys(t, events, []string{"resource:disk:/a", "resource:disk:/z"})
	for _, event := range events {
		if event.Kind != mailer.Alert {
			t.Errorf("event %q kind = %q, want ALERT", event.Key, event.Kind)
		}
		if event.Hostname != "monitor-01" || event.BaseURL != "http://127.0.0.1:8317" {
			t.Errorf("event host/base URL = %q/%q", event.Hostname, event.BaseURL)
		}
		if !event.Timestamp.Equal(testNow.UTC()) || event.Timestamp.Location() != time.UTC {
			t.Errorf("event timestamp = %v, want %v UTC", event.Timestamp, testNow.UTC())
		}
	}
	if got, want := events[0].Details, "filesystem=ext4\nused=85"; got != want {
		t.Errorf("sorted details = %q, want %q", got, want)
	}
	if got := store.SaveCalls(); got != 1 {
		t.Fatalf("Save calls = %d, want 1", got)
	}
	for _, condition := range batch.Conditions {
		record, ok := store.Record(condition.Key)
		if !ok || record.Scope != rule.ScopeDisk || !record.ActivatedAt.Equal(testNow.UTC()) {
			t.Errorf("record %q = %+v, ok=%v", condition.Key, record, ok)
		}
	}

	// Ongoing conditions neither resend nor rewrite state.
	if err := manager.Reconcile(context.Background(), batch); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if got := len(sender.Events()); got != 2 {
		t.Errorf("events after ongoing batch = %d, want 2", got)
	}
	if got := store.SaveCalls(); got != 1 {
		t.Errorf("Save calls after ongoing batch = %d, want 1", got)
	}
}

func TestManagerEscapesControlCharactersInMailObject(t *testing.T) {
	t.Parallel()

	sender := newFakeSender()
	manager := newTestManager(sender, newFakeStore(), false)
	batch := rule.Batch{Scope: rule.ScopeDisk, Complete: true, Conditions: []rule.Condition{
		condition("resource:disk:/line\nbreak", rule.ScopeDisk, "disk /line\nbreak\tusage 90%"),
	}}
	if err := manager.Reconcile(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	event := sender.Events()[0]
	if strings.ContainsAny(event.Object, "\r\n\t") || !strings.Contains(event.Object, `\n`) || !strings.Contains(event.Object, `\t`) {
		t.Fatalf("event object = %q", event.Object)
	}
}

func TestManagerHealthyWithoutRecoveryMailDeletesActive(t *testing.T) {
	t.Parallel()

	sender := newFakeSender()
	store := newFakeStore(activeRecord("resource:memory", rule.ScopeMemory))
	manager := newTestManager(sender, store, false)
	if err := manager.Reconcile(context.Background(), rule.Batch{Scope: rule.ScopeMemory, Complete: true}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, ok := store.Record("resource:memory"); ok {
		t.Fatal("active record was not deleted")
	}
	if got := len(sender.Events()); got != 0 {
		t.Errorf("events = %d, want 0", got)
	}
	if got := store.SaveCalls(); got != 1 {
		t.Errorf("Save calls = %d, want 1", got)
	}
}

func TestManagerRecoveryMailSucceedsOnceThenDeletes(t *testing.T) {
	t.Parallel()

	record := activeRecord("auth:7", rule.ScopeAuth)
	record.Summary = "auth user@example.com unavailable"
	record.Threshold = "active and available"
	record.Details = map[string]string{"provider": "openai", "email": "user@example.com"}
	sender := newFakeSender()
	store := newFakeStore(record)
	manager := newTestManager(sender, store, true)
	healthy := rule.Batch{Scope: rule.ScopeAuth, Complete: true}

	if err := manager.Reconcile(context.Background(), healthy); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, ok := store.Record(record.Key); ok {
		t.Fatal("record remains active after recovery send")
	}
	events := sender.Events()
	if len(events) != 1 {
		t.Fatalf("events = %#v, want one recovery", events)
	}
	event := events[0]
	if event.Kind != mailer.Recovery || event.Key != record.Key || event.Current != "recovered" {
		t.Errorf("recovery event = %+v", event)
	}
	if event.Object != record.Summary+" recovered" || event.Details != "email=user@example.com\nprovider=openai" {
		t.Errorf("recovery object/details = %q/%q", event.Object, event.Details)
	}
	if err := manager.Reconcile(context.Background(), healthy); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if got := len(sender.Events()); got != 1 {
		t.Errorf("events after second healthy batch = %d, want 1", got)
	}
	if got := store.SaveCalls(); got != 1 {
		t.Errorf("Save calls = %d, want 1", got)
	}
}

func TestManagerAlertSendFailureRetriesAndDoesNotBlockOtherKeys(t *testing.T) {
	t.Parallel()

	sendError := errors.New("SMTP unavailable")
	sender := newFakeSender()
	sender.Fail(mailer.Alert, "auth:a", sendError)
	store := newFakeStore()
	manager := newTestManager(sender, store, false)
	batch := rule.Batch{Scope: rule.ScopeAuth, Complete: true, Conditions: []rule.Condition{
		condition("auth:b", rule.ScopeAuth, "auth b unavailable"),
		condition("auth:a", rule.ScopeAuth, "auth a unavailable"),
	}}

	if err := manager.Reconcile(context.Background(), batch); !errors.Is(err, sendError) {
		t.Fatalf("Reconcile() error = %v, want send error", err)
	}
	assertEventKeys(t, sender.Events(), []string{"auth:a", "auth:b"})
	if _, ok := store.Record("auth:a"); ok {
		t.Error("failed alert was activated")
	}
	if _, ok := store.Record("auth:b"); !ok {
		t.Error("successful later alert was not activated")
	}
	if got := store.SaveCalls(); got != 1 {
		t.Errorf("Save calls = %d, want 1", got)
	}

	if err := manager.Reconcile(context.Background(), batch); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	assertEventKeys(t, sender.Events(), []string{"auth:a", "auth:b", "auth:a"})
	if _, ok := store.Record("auth:a"); !ok {
		t.Error("retried alert was not activated")
	}
	if got := store.SaveCalls(); got != 2 {
		t.Errorf("Save calls after retry = %d, want 2", got)
	}
}

func TestManagerRecoveryFailureRetriesAndDoesNotBlockOtherKeys(t *testing.T) {
	t.Parallel()

	sendError := errors.New("recovery send failed")
	sender := newFakeSender()
	sender.Fail(mailer.Recovery, "auth:a", sendError)
	store := newFakeStore(
		activeRecord("auth:b", rule.ScopeAuth),
		activeRecord("auth:a", rule.ScopeAuth),
	)
	manager := newTestManager(sender, store, true)
	healthy := rule.Batch{Scope: rule.ScopeAuth, Complete: true}

	if err := manager.Reconcile(context.Background(), healthy); !errors.Is(err, sendError) {
		t.Fatalf("Reconcile() error = %v, want send error", err)
	}
	assertEventKeys(t, sender.Events(), []string{"auth:a", "auth:b"})
	if _, ok := store.Record("auth:a"); !ok {
		t.Error("failed recovery removed active key")
	}
	if _, ok := store.Record("auth:b"); ok {
		t.Error("successful later recovery retained active key")
	}
	if got := store.SaveCalls(); got != 1 {
		t.Errorf("Save calls = %d, want 1", got)
	}

	if err := manager.Reconcile(context.Background(), healthy); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	assertEventKeys(t, sender.Events(), []string{"auth:a", "auth:b", "auth:a"})
	if _, ok := store.Record("auth:a"); ok {
		t.Error("successful recovery retry retained active key")
	}
}

func TestManagerIncompleteBatchAlertsKnownKeysWithoutRecoveringMissing(t *testing.T) {
	t.Parallel()

	sender := newFakeSender()
	store := newFakeStore(
		activeRecord("resource:disk:/missing", rule.ScopeDisk),
		activeRecord("auth:other", rule.ScopeAuth),
	)
	manager := newTestManager(sender, store, false)
	batch := rule.Batch{
		Scope:      rule.ScopeDisk,
		Complete:   false,
		Conditions: []rule.Condition{condition("resource:disk:/known", rule.ScopeDisk, "known disk full")},
		Errors:     []error{errors.New("statfs missing")},
	}
	if err := manager.Reconcile(context.Background(), batch); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	assertEventKeys(t, sender.Events(), []string{"resource:disk:/known"})
	for _, key := range []string{"resource:disk:/known", "resource:disk:/missing", "auth:other"} {
		if _, ok := store.Record(key); !ok {
			t.Errorf("record %q missing after incomplete reconcile", key)
		}
	}
	if got := store.SaveCalls(); got != 1 {
		t.Errorf("Save calls = %d, want 1", got)
	}
}

func TestManagerOperationsAcrossAlertsAndRecoveriesAreSortedAndSaveOnce(t *testing.T) {
	t.Parallel()

	sender := newFakeSender()
	store := newFakeStore(activeRecord("auth:m", rule.ScopeAuth))
	manager := newTestManager(sender, store, true)
	batch := rule.Batch{Scope: rule.ScopeAuth, Complete: true, Conditions: []rule.Condition{
		condition("auth:z", rule.ScopeAuth, "auth z unavailable"),
		condition("auth:a", rule.ScopeAuth, "auth a unavailable"),
	}}
	if err := manager.Reconcile(context.Background(), batch); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	events := sender.Events()
	assertEventKeys(t, events, []string{"auth:a", "auth:m", "auth:z"})
	if kinds := []mailer.Kind{events[0].Kind, events[1].Kind, events[2].Kind}; !reflect.DeepEqual(kinds, []mailer.Kind{mailer.Alert, mailer.Recovery, mailer.Alert}) {
		t.Errorf("event kinds = %v", kinds)
	}
	if got := store.SaveCalls(); got != 1 {
		t.Errorf("Save calls = %d, want 1", got)
	}
}

func TestManagerSaveFailureKeepsInMemoryMutationAndPreventsResend(t *testing.T) {
	t.Parallel()

	saveError := errors.New("disk full")
	sender := newFakeSender()
	store := newFakeStore()
	store.SetSaveError(saveError)
	manager := newTestManager(sender, store, false)
	batch := rule.Batch{Scope: rule.ScopeMemory, Complete: true, Conditions: []rule.Condition{
		condition("resource:memory", rule.ScopeMemory, "memory usage 90%"),
	}}
	if err := manager.Reconcile(context.Background(), batch); !errors.Is(err, saveError) {
		t.Fatalf("Reconcile() error = %v, want save error", err)
	}
	if _, ok := store.Record("resource:memory"); !ok {
		t.Fatal("Save failure rolled back in-memory active record")
	}
	store.SetSaveError(nil)
	if err := manager.Reconcile(context.Background(), batch); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if got := len(sender.Events()); got != 1 {
		t.Errorf("events = %d, want no resend after Save failure", got)
	}
	if got := store.SaveCalls(); got != 2 {
		t.Errorf("Save calls = %d, want one persistence retry without resending", got)
	}
}

func TestManagerPutFailureRetriesAndContinues(t *testing.T) {
	t.Parallel()

	putError := errors.New("put failed")
	sender := newFakeSender()
	store := newFakeStore()
	store.FailPut("network:a", putError)
	manager := newTestManager(sender, store, false)
	batch := rule.Batch{Scope: rule.ScopeNetwork, Complete: true, Conditions: []rule.Condition{
		condition("network:b", rule.ScopeNetwork, "network b"),
		condition("network:a", rule.ScopeNetwork, "network a"),
	}}
	if err := manager.Reconcile(context.Background(), batch); !errors.Is(err, putError) {
		t.Fatalf("Reconcile() error = %v, want put error", err)
	}
	assertEventKeys(t, sender.Events(), []string{"network:a", "network:b"})
	if _, ok := store.Record("network:b"); !ok {
		t.Error("later key did not activate after Put failure")
	}
	if _, ok := store.Record("network:a"); ok {
		t.Error("failed Put key became active")
	}
	if err := manager.Reconcile(context.Background(), batch); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	assertEventKeys(t, sender.Events(), []string{"network:a", "network:b", "network:a"})
}

func TestManagerConcurrentReconcileDoesNotDuplicateAlert(t *testing.T) {
	t.Parallel()

	sender := newFakeSender()
	store := newFakeStore()
	manager := newTestManager(sender, store, false)
	batch := rule.Batch{Scope: rule.ScopeMemory, Complete: true, Conditions: []rule.Condition{
		condition("resource:memory", rule.ScopeMemory, "memory usage 90%"),
	}}
	start := make(chan struct{})
	errorsSeen := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errorsSeen <- manager.Reconcile(context.Background(), batch)
		}()
	}
	close(start)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Errorf("Reconcile() error = %v", err)
		}
	}
	if got := len(sender.Events()); got != 1 {
		t.Errorf("events = %d, want one alert", got)
	}
	if got := store.SaveCalls(); got != 1 {
		t.Errorf("Save calls = %d, want 1", got)
	}
}

func TestManagerRejectsInvalidBatchBeforeSideEffects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		batch rule.Batch
	}{
		{name: "empty scope", batch: rule.Batch{Complete: true}},
		{name: "empty key", batch: rule.Batch{Scope: rule.ScopeAuth, Complete: true, Conditions: []rule.Condition{{Scope: rule.ScopeAuth}}}},
		{name: "scope mismatch", batch: rule.Batch{Scope: rule.ScopeAuth, Complete: true, Conditions: []rule.Condition{{Key: "auth:1", Scope: rule.ScopeDisk}}}},
		{name: "duplicate key", batch: rule.Batch{Scope: rule.ScopeAuth, Complete: true, Conditions: []rule.Condition{{Key: "auth:1", Scope: rule.ScopeAuth}, {Key: "auth:1", Scope: rule.ScopeAuth}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sender := newFakeSender()
			store := newFakeStore()
			manager := newTestManager(sender, store, false)
			if err := manager.Reconcile(context.Background(), tt.batch); err == nil {
				t.Fatal("Reconcile() error = nil, want validation error")
			}
			if len(sender.Events()) != 0 || store.SaveCalls() != 0 {
				t.Fatal("invalid batch caused side effects")
			}
		})
	}
}

var testNow = time.Date(2026, time.July, 10, 9, 8, 7, 0, time.FixedZone("CST", 8*60*60))

func newTestManager(sender Sender, store Store, sendRecovery bool) *Manager {
	manager := NewManager(sender, store, "monitor-01", "http://127.0.0.1:8317", sendRecovery)
	manager.now = func() time.Time { return testNow }
	return manager
}

func condition(key, scope, summary string) rule.Condition {
	return rule.Condition{
		Key:       key,
		Scope:     scope,
		Summary:   summary,
		Current:   "unhealthy",
		Threshold: "healthy",
	}
}

func activeRecord(key, scope string) state.Record {
	return state.Record{
		Key:         key,
		Scope:       scope,
		Summary:     key + " unhealthy",
		Current:     "unhealthy",
		Threshold:   "healthy",
		ActivatedAt: testNow.Add(-time.Hour).UTC(),
	}
}

func assertEventKeys(t *testing.T, events []mailer.Event, want []string) {
	t.Helper()
	got := make([]string, len(events))
	for i := range events {
		got[i] = events[i].Key
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event keys = %v, want %v", got, want)
	}
}

type fakeSender struct {
	mu       sync.Mutex
	events   []mailer.Event
	failures map[string][]error
}

func newFakeSender() *fakeSender {
	return &fakeSender{failures: make(map[string][]error)}
}

func (s *fakeSender) Send(_ context.Context, event mailer.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	key := eventFailureKey(event.Kind, event.Key)
	queued := s.failures[key]
	if len(queued) == 0 {
		return nil
	}
	err := queued[0]
	if len(queued) == 1 {
		delete(s.failures, key)
	} else {
		s.failures[key] = queued[1:]
	}
	return err
}

func (s *fakeSender) Fail(kind mailer.Kind, key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	failureKey := eventFailureKey(kind, key)
	s.failures[failureKey] = append(s.failures[failureKey], err)
}

func (s *fakeSender) Events() []mailer.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mailer.Event(nil), s.events...)
}

func eventFailureKey(kind mailer.Kind, key string) string {
	return string(kind) + "\x00" + key
}

type fakeStore struct {
	mu          sync.Mutex
	records     map[string]state.Record
	putFailures map[string][]error
	saveError   error
	saveCalls   int
}

func newFakeStore(records ...state.Record) *fakeStore {
	store := &fakeStore{
		records:     make(map[string]state.Record, len(records)),
		putFailures: make(map[string][]error),
	}
	for _, record := range records {
		store.records[record.Key] = cloneRecord(record)
	}
	return store
}

func (s *fakeStore) ByScope(scope string) []state.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var records []state.Record
	for _, record := range s.records {
		if record.Scope == scope {
			records = append(records, cloneRecord(record))
		}
	}
	// Return reverse order to ensure Manager, rather than the fake, establishes
	// the operation ordering.
	sort.Slice(records, func(i, j int) bool { return records[i].Key > records[j].Key })
	return records
}

func (s *fakeStore) Put(record state.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	queued := s.putFailures[record.Key]
	if len(queued) != 0 {
		err := queued[0]
		if len(queued) == 1 {
			delete(s.putFailures, record.Key)
		} else {
			s.putFailures[record.Key] = queued[1:]
		}
		return err
	}
	s.records[record.Key] = cloneRecord(record)
	return nil
}

func (s *fakeStore) Delete(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[key]; !ok {
		return false
	}
	delete(s.records, key)
	return true
}

func (s *fakeStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCalls++
	return s.saveError
}

func (s *fakeStore) Record(key string) (state.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	return cloneRecord(record), ok
}

func (s *fakeStore) SaveCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveCalls
}

func (s *fakeStore) SetSaveError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveError = err
}

func (s *fakeStore) FailPut(key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putFailures[key] = append(s.putFailures[key], err)
}

func cloneRecord(record state.Record) state.Record {
	if record.Details != nil {
		record.Details = cloneDetails(record.Details)
	}
	return record
}

func TestFormatDetailsIsStable(t *testing.T) {
	t.Parallel()
	got := formatDetails(map[string]string{"z": "last", "a": "first", "m": "middle"})
	if want := "a=first\nm=middle\nz=last"; got != want {
		t.Fatalf("formatDetails() = %q, want %q", got, want)
	}
	if got := formatDetails(nil); got != "" {
		t.Fatalf("formatDetails(nil) = %q", got)
	}
}

func TestManagerValidationDependenciesAndContext(t *testing.T) {
	t.Parallel()
	validBatch := rule.Batch{Scope: rule.ScopeHealth, Complete: true}
	validSender := newFakeSender()
	validStore := newFakeStore()
	for _, test := range []struct {
		name    string
		manager *Manager
		ctx     context.Context
	}{
		{name: "nil manager", ctx: context.Background()},
		{name: "nil context", manager: NewManager(validSender, validStore, "host", "url", false)},
		{name: "nil sender", manager: NewManager(nil, validStore, "host", "url", false), ctx: context.Background()},
		{name: "nil store", manager: NewManager(validSender, nil, "host", "url", false), ctx: context.Background()},
		{name: "empty host", manager: NewManager(validSender, validStore, "", "url", false), ctx: context.Background()},
		{name: "empty URL", manager: NewManager(validSender, validStore, "host", "", false), ctx: context.Background()},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.manager.Reconcile(test.ctx, validBatch); err == nil {
				t.Fatal("Reconcile() error = nil, want validation error")
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	manager := NewManager(validSender, validStore, "host", "url", false)
	if err := manager.Reconcile(ctx, validBatch); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile(canceled) error = %v, want context.Canceled", err)
	}
}

func TestErrorsMentionKeysWithoutConditionDetails(t *testing.T) {
	t.Parallel()
	secret := "secret-status-message"
	sendError := errors.New("send failed")
	sender := newFakeSender()
	sender.Fail(mailer.Alert, "auth:1", sendError)
	store := newFakeStore()
	manager := newTestManager(sender, store, false)
	condition := condition("auth:1", rule.ScopeAuth, "account unavailable")
	condition.Details = map[string]string{"status_message": secret}
	err := manager.Reconcile(context.Background(), rule.Batch{Scope: rule.ScopeAuth, Complete: true, Conditions: []rule.Condition{condition}})
	if !errors.Is(err, sendError) || !strings.Contains(err.Error(), "auth:1") {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Reconcile() error leaks condition details: %v", err)
	}
}
