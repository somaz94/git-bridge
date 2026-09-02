package consumer

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"git-bridge/internal/config"
	"git-bridge/internal/mirror"
)

const (
	sqsMaxMessages     = 10
	sqsWaitTimeSeconds = 20
	sqsErrorRetryDelay = 5 * time.Second

	eventReferenceDeleted = "referenceDeleted" // the CodeCommit event type for a ref deletion
	refTypeTag            = "tag"              // the ref-kind label for a tag
	refsTagsPrefix        = "refs/tags/"
	refsHeadsPrefix       = "refs/heads/"
)

// CodeCommitEvent represents the EventBridge event from CodeCommit.
type CodeCommitEvent struct {
	Detail struct {
		RepositoryName string `json:"repositoryName"`
		ReferenceName  string `json:"referenceName"`
		ReferenceType  string `json:"referenceType"`
		Event          string `json:"event"`
	} `json:"detail"`
}

// sqsClient defines the SQS operations used by the consumer.
type sqsClient interface {
	ReceiveMessage(ctx context.Context, input *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, input *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// Syncer performs mirror sync operations triggered by source-side events.
type Syncer interface {
	Sync(ctx context.Context, repoName string, meta mirror.EventMeta) error
	SyncDelete(ctx context.Context, repoName, refType, refName string) error
}

// SQS polls an SQS queue for mirror events.
type SQS struct {
	name      string
	client    sqsClient
	queueURL  string
	mirrorSvc Syncer
	// visibilityTimeout hides a received message from other consumers while the
	// batch is handled. config guarantees it is at least mirror.timeout_seconds.
	visibilityTimeout int32
}

// NewSQS creates a new SQS consumer.
func NewSQS(cfg config.ConsumerConfig, mirrorSvc Syncer) (*SQS, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}

	if cfg.Credentials.AccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				cfg.Credentials.AccessKey,
				cfg.Credentials.SecretKey,
				"",
			),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, err
	}

	name := cfg.Name
	if name == "" {
		name = "default"
	}

	return &SQS{
		name:              name,
		client:            sqs.NewFromConfig(awsCfg),
		queueURL:          cfg.QueueURL,
		mirrorSvc:         mirrorSvc,
		visibilityTimeout: int32(cfg.VisibilityTimeoutSeconds),
	}, nil
}

// Start begins long-polling the SQS queue.
// Start polls until pollCtx is cancelled.
//
// work is a separate context for the mirroring a message triggers, and the two
// are not interchangeable. Cancelling pollCtx means "stop taking new messages";
// the message already being handled keeps running under work so shutdown can
// let it finish rather than killing its git command mid-fetch. Start does not
// return until that message is done, which is what makes waiting on it a real
// drain.
func (s *SQS) Start(pollCtx, work context.Context) {
	slog.Info("SQS consumer started", "name", s.name, "queue", s.queueURL)

	for {
		select {
		case <-pollCtx.Done():
			slog.Info("SQS consumer stopped", "name", s.name)
			return
		default:
			s.poll(pollCtx, work)
		}
	}
}

func (s *SQS) poll(ctx, work context.Context) {
	result, err := s.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            &s.queueURL,
		MaxNumberOfMessages: sqsMaxMessages,
		WaitTimeSeconds:     sqsWaitTimeSeconds,
		VisibilityTimeout:   s.visibilityTimeout,
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Error("SQS receive error", "name", s.name, "error", err)
		time.Sleep(sqsErrorRetryDelay)
		return
	}

	for _, msg := range result.Messages {
		s.handleMessage(work, msg)
	}
}

func (s *SQS) handleMessage(ctx context.Context, msg types.Message) {
	logger := slog.With("message_id", *msg.MessageId)

	var event CodeCommitEvent
	if err := json.Unmarshal([]byte(*msg.Body), &event); err != nil {
		logger.Error("failed to parse event", "error", err)
		s.deleteMessage(ctx, msg)
		return
	}

	repo := event.Detail.RepositoryName
	ref := event.Detail.ReferenceName
	refType := event.Detail.ReferenceType
	eventType := event.Detail.Event
	logger = logger.With("repo", repo, "ref", ref, "event", eventType)
	logger.Info("received mirror event")

	fullRef := refsHeadsPrefix + ref
	if refType == refTypeTag {
		fullRef = refsTagsPrefix + ref
	}

	var err error
	if eventType == eventReferenceDeleted {
		err = s.mirrorSvc.SyncDelete(ctx, repo, refType, ref)
	} else {
		err = s.mirrorSvc.Sync(ctx, repo, mirror.EventMeta{Ref: fullRef, Source: mirror.SourceSQS})
	}

	if err != nil {
		logger.Error("mirror sync failed", "error", err)
		// Don't delete - will retry via SQS visibility timeout
		return
	}

	logger.Info("mirror sync completed")
	s.deleteMessage(ctx, msg)
}

func (s *SQS) deleteMessage(ctx context.Context, msg types.Message) {
	_, err := s.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      &s.queueURL,
		ReceiptHandle: msg.ReceiptHandle,
	})
	if err != nil {
		slog.Error("failed to delete SQS message", "message_id", aws.ToString(msg.MessageId), "error", err)
	}
}
