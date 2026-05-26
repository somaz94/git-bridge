package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"git-bridge/internal/config"
	"git-bridge/internal/mirror"
)

// mockSQSClient implements sqsClient for testing.
type mockSQSClient struct {
	receiveFunc func(ctx context.Context, input *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	deleteFunc  func(ctx context.Context, input *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	deleteCalls int
	deleteIDs   []string
}

func (m *mockSQSClient) ReceiveMessage(ctx context.Context, input *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	if m.receiveFunc != nil {
		return m.receiveFunc(ctx, input, opts...)
	}
	return &sqs.ReceiveMessageOutput{}, nil
}

func (m *mockSQSClient) DeleteMessage(ctx context.Context, input *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	m.deleteCalls++
	if input.ReceiptHandle != nil {
		m.deleteIDs = append(m.deleteIDs, *input.ReceiptHandle)
	}
	if m.deleteFunc != nil {
		return m.deleteFunc(ctx, input, opts...)
	}
	return &sqs.DeleteMessageOutput{}, nil
}

// mockSyncer records Sync and SyncDelete calls and returns configurable errors.
type mockSyncer struct {
	syncCalls     []string
	syncMetas     []mirror.EventMeta
	syncErr       error
	deleteCalls   []deleteCall
	syncDeleteErr error
}

type deleteCall struct {
	RepoName string
	RefType  string
	RefName  string
}

func (m *mockSyncer) Sync(_ context.Context, repoName string, meta mirror.EventMeta) error {
	m.syncCalls = append(m.syncCalls, repoName)
	m.syncMetas = append(m.syncMetas, meta)
	return m.syncErr
}

func (m *mockSyncer) SyncDelete(_ context.Context, repoName, refType, refName string) error {
	m.deleteCalls = append(m.deleteCalls, deleteCall{RepoName: repoName, RefType: refType, RefName: refName})
	return m.syncDeleteErr
}

// makeEvent creates a JSON-encoded CodeCommit event.
func makeEvent(repoName, ref string) string {
	return makeEventFull(repoName, ref, "branch", "referenceUpdated")
}

func makeDeleteEvent(repoName, ref, refType string) string {
	return makeEventFull(repoName, ref, refType, "referenceDeleted")
}

func makeEventFull(repoName, ref, refType, event string) string {
	evt := CodeCommitEvent{}
	evt.Detail.RepositoryName = repoName
	evt.Detail.ReferenceName = ref
	evt.Detail.ReferenceType = refType
	evt.Detail.Event = event
	data, _ := json.Marshal(evt)
	return string(data)
}

func makeSQSMessage(id, body string) sqstypes.Message {
	return sqstypes.Message{
		MessageId:     aws.String(id),
		Body:          aws.String(body),
		ReceiptHandle: aws.String("receipt-" + id),
	}
}

func newTestSQSConsumer(mock *mockSQSClient, syncer *mockSyncer) *SQS {
	return &SQS{
		name:      "test",
		client:    mock,
		queueURL:  "https://sqs.example.com/test-queue",
		mirrorSvc: syncer,
	}
}

// --- handleMessage tests ---

func TestHandleMessage_ValidEvent_SyncSuccess(t *testing.T) {
	syncer := &mockSyncer{}
	mock := &mockSQSClient{}
	s := newTestSQSConsumer(mock, syncer)

	body := makeEvent("my-repo", "refs/heads/main")
	msg := makeSQSMessage("msg-1", body)

	s.handleMessage(context.Background(), msg)

	if len(syncer.syncCalls) != 1 || syncer.syncCalls[0] != "my-repo" {
		t.Errorf("expected Sync('my-repo'), got %v", syncer.syncCalls)
	}
	if mock.deleteCalls != 1 {
		t.Errorf("expected 1 delete call, got %d", mock.deleteCalls)
	}
}

func TestHandleMessage_InvalidJSON(t *testing.T) {
	syncer := &mockSyncer{}
	mock := &mockSQSClient{}
	s := newTestSQSConsumer(mock, syncer)

	msg := makeSQSMessage("msg-2", "not valid json")
	s.handleMessage(context.Background(), msg)

	if len(syncer.syncCalls) != 0 {
		t.Error("should not sync on invalid JSON")
	}
	// Invalid messages should still be deleted (to avoid retry loop)
	if mock.deleteCalls != 1 {
		t.Errorf("expected delete for invalid message, got %d", mock.deleteCalls)
	}
}

func TestHandleMessage_SyncFailure_NoDelete(t *testing.T) {
	syncer := &mockSyncer{syncErr: fmt.Errorf("sync failed")}
	mock := &mockSQSClient{}
	s := newTestSQSConsumer(mock, syncer)

	body := makeEvent("my-repo", "refs/heads/main")
	msg := makeSQSMessage("msg-3", body)
	s.handleMessage(context.Background(), msg)

	if len(syncer.syncCalls) != 1 {
		t.Errorf("expected 1 sync call, got %d", len(syncer.syncCalls))
	}
	// Message should NOT be deleted on sync failure (SQS will retry)
	if mock.deleteCalls != 0 {
		t.Errorf("expected 0 delete calls on sync failure, got %d", mock.deleteCalls)
	}
}

func TestHandleMessage_MultipleMessages(t *testing.T) {
	syncer := &mockSyncer{}
	mock := &mockSQSClient{}
	s := newTestSQSConsumer(mock, syncer)

	msgs := []sqstypes.Message{
		makeSQSMessage("msg-a", makeEvent("repo-a", "refs/heads/main")),
		makeSQSMessage("msg-b", makeEvent("repo-b", "refs/heads/develop")),
	}

	for _, msg := range msgs {
		s.handleMessage(context.Background(), msg)
	}

	if len(syncer.syncCalls) != 2 {
		t.Errorf("expected 2 sync calls, got %d", len(syncer.syncCalls))
	}
	if mock.deleteCalls != 2 {
		t.Errorf("expected 2 delete calls, got %d", mock.deleteCalls)
	}
}

// --- poll tests ---

func TestPoll_ProcessesMessages(t *testing.T) {
	syncer := &mockSyncer{}
	mock := &mockSQSClient{
		receiveFunc: func(ctx context.Context, input *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
			return &sqs.ReceiveMessageOutput{
				Messages: []sqstypes.Message{
					makeSQSMessage("msg-a", makeEvent("repo-a", "refs/heads/main")),
					makeSQSMessage("msg-b", makeEvent("repo-b", "refs/heads/develop")),
				},
			}, nil
		},
	}
	s := newTestSQSConsumer(mock, syncer)

	s.poll(context.Background())

	if len(syncer.syncCalls) != 2 {
		t.Errorf("expected 2 sync calls, got %d", len(syncer.syncCalls))
	}
	if mock.deleteCalls != 2 {
		t.Errorf("expected 2 delete calls, got %d", mock.deleteCalls)
	}
}

func TestPoll_ReceiveError(t *testing.T) {
	syncer := &mockSyncer{}
	mock := &mockSQSClient{
		receiveFunc: func(ctx context.Context, input *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
			return nil, fmt.Errorf("network error")
		},
	}
	s := newTestSQSConsumer(mock, syncer)

	// Should not panic; just log and return (sleep skipped in test due to short context)
	s.poll(context.Background())

	if len(syncer.syncCalls) != 0 {
		t.Error("should not sync on receive error")
	}
}

func TestPoll_ContextCancelled(t *testing.T) {
	syncer := &mockSyncer{}
	mock := &mockSQSClient{
		receiveFunc: func(ctx context.Context, input *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
			return nil, ctx.Err()
		},
	}
	s := newTestSQSConsumer(mock, syncer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.poll(ctx)

	if len(syncer.syncCalls) != 0 {
		t.Error("should not sync when context is cancelled")
	}
}

func TestPoll_EmptyMessages(t *testing.T) {
	syncer := &mockSyncer{}
	mock := &mockSQSClient{
		receiveFunc: func(ctx context.Context, input *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
			return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{}}, nil
		},
	}
	s := newTestSQSConsumer(mock, syncer)

	s.poll(context.Background())

	if len(syncer.syncCalls) != 0 {
		t.Error("should not sync with no messages")
	}
}

// --- Start tests ---

func TestStart_StopsOnContextCancel(t *testing.T) {
	syncer := &mockSyncer{}
	pollCount := 0
	mock := &mockSQSClient{
		receiveFunc: func(ctx context.Context, input *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
			pollCount++
			return &sqs.ReceiveMessageOutput{}, nil
		},
	}
	s := newTestSQSConsumer(mock, syncer)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	s.Start(ctx)

	if pollCount == 0 {
		t.Error("expected at least one poll before context cancellation")
	}
}

// --- deleteMessage tests ---

func TestDeleteMessage_Success(t *testing.T) {
	mock := &mockSQSClient{}
	s := newTestSQSConsumer(mock, &mockSyncer{})

	msg := makeSQSMessage("msg-ok", makeEvent("repo", "refs/heads/main"))
	s.deleteMessage(context.Background(), msg)

	if mock.deleteCalls != 1 {
		t.Errorf("expected 1 delete call, got %d", mock.deleteCalls)
	}
}

func TestDeleteMessage_Error(t *testing.T) {
	mock := &mockSQSClient{
		deleteFunc: func(ctx context.Context, input *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
			return nil, fmt.Errorf("delete failed")
		},
	}
	s := newTestSQSConsumer(mock, &mockSyncer{})

	// Should not panic on delete error
	msg := makeSQSMessage("msg-err", makeEvent("repo", "refs/heads/main"))
	s.deleteMessage(context.Background(), msg)
}

// --- CodeCommitEvent tests ---

func TestCodeCommitEvent_Parsing(t *testing.T) {
	raw := `{"detail":{"repositoryName":"my-repo","referenceName":"refs/heads/main","referenceType":"branch","event":"referenceUpdated"}}`
	var event CodeCommitEvent
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if event.Detail.RepositoryName != "my-repo" {
		t.Errorf("expected my-repo, got %q", event.Detail.RepositoryName)
	}
	if event.Detail.ReferenceName != "refs/heads/main" {
		t.Errorf("expected refs/heads/main, got %q", event.Detail.ReferenceName)
	}
}

func TestNewSQS_WithCredentials(t *testing.T) {
	cfg := config.ConsumerConfig{
		Type:     "sqs",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456/test-queue",
		Region:   "us-east-1",
	}
	cfg.Credentials.AccessKey = "AKIATEST"
	cfg.Credentials.SecretKey = "secrettest"

	syncer := &mockSyncer{}
	s, err := NewSQS(cfg, syncer)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.queueURL != cfg.QueueURL {
		t.Errorf("queueURL = %q, want %q", s.queueURL, cfg.QueueURL)
	}
	if s.client == nil {
		t.Error("client should not be nil")
	}
}

func TestNewSQS_WithoutCredentials(t *testing.T) {
	cfg := config.ConsumerConfig{
		Type:     "sqs",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456/test-queue",
		Region:   "us-east-1",
	}
	// No credentials set - uses default AWS credential chain

	syncer := &mockSyncer{}
	s, err := NewSQS(cfg, syncer)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.queueURL != cfg.QueueURL {
		t.Errorf("queueURL = %q, want %q", s.queueURL, cfg.QueueURL)
	}
}

func TestHandleMessage_DeleteBranch(t *testing.T) {
	syncer := &mockSyncer{}
	mock := &mockSQSClient{}
	s := newTestSQSConsumer(mock, syncer)

	body := makeDeleteEvent("my-repo", "feature-branch", "branch")
	msg := makeSQSMessage("msg-del-1", body)
	s.handleMessage(context.Background(), msg)

	if len(syncer.syncCalls) != 0 {
		t.Error("should not call Sync for delete events")
	}
	if len(syncer.deleteCalls) != 1 {
		t.Fatalf("expected 1 SyncDelete call, got %d", len(syncer.deleteCalls))
	}
	dc := syncer.deleteCalls[0]
	if dc.RepoName != "my-repo" || dc.RefType != "branch" || dc.RefName != "feature-branch" {
		t.Errorf("unexpected delete call: %+v", dc)
	}
	if mock.deleteCalls != 1 {
		t.Errorf("expected message deleted, got %d", mock.deleteCalls)
	}
}

func TestHandleMessage_DeleteTag(t *testing.T) {
	syncer := &mockSyncer{}
	mock := &mockSQSClient{}
	s := newTestSQSConsumer(mock, syncer)

	body := makeDeleteEvent("my-repo", "v1.0.0", "tag")
	msg := makeSQSMessage("msg-del-2", body)
	s.handleMessage(context.Background(), msg)

	if len(syncer.deleteCalls) != 1 {
		t.Fatalf("expected 1 SyncDelete call, got %d", len(syncer.deleteCalls))
	}
	dc := syncer.deleteCalls[0]
	if dc.RefType != "tag" || dc.RefName != "v1.0.0" {
		t.Errorf("unexpected delete call: %+v", dc)
	}
}

func TestHandleMessage_DeleteFailure_NoMessageDelete(t *testing.T) {
	syncer := &mockSyncer{syncDeleteErr: fmt.Errorf("delete failed")}
	mock := &mockSQSClient{}
	s := newTestSQSConsumer(mock, syncer)

	body := makeDeleteEvent("my-repo", "old-branch", "branch")
	msg := makeSQSMessage("msg-del-3", body)
	s.handleMessage(context.Background(), msg)

	if mock.deleteCalls != 0 {
		t.Errorf("should not delete SQS message on failure, got %d", mock.deleteCalls)
	}
}

func TestNewSQS_DefaultName(t *testing.T) {
	cfg := config.ConsumerConfig{
		Type:     "sqs",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456/test-queue",
		Region:   "us-east-1",
		Name:     "",
	}

	syncer := &mockSyncer{}
	s, err := NewSQS(cfg, syncer)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.name != "default" {
		t.Errorf("name = %q, want default", s.name)
	}
}

func TestNewSQS_CustomName(t *testing.T) {
	cfg := config.ConsumerConfig{
		Type:     "sqs",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123456/test-queue",
		Region:   "us-east-1",
		Name:     "my-consumer",
	}

	syncer := &mockSyncer{}
	s, err := NewSQS(cfg, syncer)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.name != "my-consumer" {
		t.Errorf("name = %q, want my-consumer", s.name)
	}
}

func TestCodeCommitEvent_EmptyDetail(t *testing.T) {
	raw := `{"detail":{}}`
	var event CodeCommitEvent
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if event.Detail.RepositoryName != "" {
		t.Errorf("expected empty repo name, got %q", event.Detail.RepositoryName)
	}
}

// --- EventMeta ref construction tests ---

func TestHandleMessage_BranchEvent_PassesRefMeta(t *testing.T) {
	msgBody := makeEventFull("my-repo", "main", "branch", "referenceCreated")
	syncer := &mockSyncer{}
	s := newTestSQSConsumer(&mockSQSClient{}, syncer)
	s.handleMessage(context.Background(), makeSQSMessage("ref-1", msgBody))

	if len(syncer.syncMetas) != 1 {
		t.Fatalf("expected 1 sync meta, got %d", len(syncer.syncMetas))
	}
	if syncer.syncMetas[0].Ref != "refs/heads/main" {
		t.Errorf("expected ref 'refs/heads/main', got %q", syncer.syncMetas[0].Ref)
	}
}

func TestHandleMessage_TagEvent_PassesRefMeta(t *testing.T) {
	msgBody := makeEventFull("my-repo", "v1.0.0", "tag", "referenceCreated")
	syncer := &mockSyncer{}
	s := newTestSQSConsumer(&mockSQSClient{}, syncer)
	s.handleMessage(context.Background(), makeSQSMessage("ref-2", msgBody))

	if len(syncer.syncMetas) != 1 {
		t.Fatalf("expected 1 sync meta, got %d", len(syncer.syncMetas))
	}
	if syncer.syncMetas[0].Ref != "refs/tags/v1.0.0" {
		t.Errorf("expected ref 'refs/tags/v1.0.0', got %q", syncer.syncMetas[0].Ref)
	}
}

func TestHandleMessage_DeleteEvent_NoSyncMeta(t *testing.T) {
	msgBody := makeDeleteEvent("my-repo", "old-branch", "branch")
	syncer := &mockSyncer{}
	s := newTestSQSConsumer(&mockSQSClient{}, syncer)
	s.handleMessage(context.Background(), makeSQSMessage("ref-3", msgBody))

	if len(syncer.syncMetas) != 0 {
		t.Errorf("expected no sync meta for delete event, got %d", len(syncer.syncMetas))
	}
	if len(syncer.deleteCalls) != 1 {
		t.Errorf("expected 1 delete call, got %d", len(syncer.deleteCalls))
	}
}
