package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"healthAgent/internal/stt"
)

// ---------------------------------------------------------------------------
// 内存版仓储：只保留断言需要的状态，不模拟 SQL 细节。
// ---------------------------------------------------------------------------

type fakeFeynmanRepository struct {
	mu                 sync.Mutex
	attempts           map[string]*FeynmanAttemptDetail  // key: attempt_id
	idempotency        map[string]string                 // key: userID + "\x00" + key -> attempt_id
	knowledgePointUser map[string]string                 // key: knowledge_point_id -> user_id（模拟外键归属校验）
	rubrics            map[string][]KnowledgePointRubric // key: knowledge_point_id
	currentRubric      map[string]string                 // key: knowledge_point_id -> rubric_id
	audioTasks         map[string]*FeynmanAudioTask      // key: audio_task_id
	audioHashes        map[string]string                 // key: attempt_id + hash -> audio_task_id
	sequence           int
}

func newFakeFeynmanRepository() *fakeFeynmanRepository {
	return &fakeFeynmanRepository{
		attempts:           map[string]*FeynmanAttemptDetail{},
		idempotency:        map[string]string{},
		knowledgePointUser: map[string]string{},
		rubrics:            map[string][]KnowledgePointRubric{},
		currentRubric:      map[string]string{},
		audioTasks:         map[string]*FeynmanAudioTask{},
		audioHashes:        map[string]string{},
	}
}

func (r *fakeFeynmanRepository) nextSeq() int {
	r.sequence++
	return r.sequence
}

func (r *fakeFeynmanRepository) FindAttemptByIdempotencyKey(_ context.Context, userID, idempotencyKey string) (FeynmanAttemptDetail, bool, error) {
	attemptID, found := r.idempotency[userID+"\x00"+idempotencyKey]
	if !found {
		return FeynmanAttemptDetail{}, false, nil
	}
	return *r.attempts[attemptID], true, nil
}

func (r *fakeFeynmanRepository) CreateAttempt(_ context.Context, params CreateFeynmanAttemptParams) (FeynmanAttemptDetail, error) {
	owner, exists := r.knowledgePointUser[params.KnowledgePointID]
	if !exists || owner != params.UserID {
		return FeynmanAttemptDetail{}, ErrFeynmanKnowledgePointNotFound
	}
	if _, conflict := r.idempotency[params.UserID+"\x00"+params.IdempotencyKey]; conflict {
		return FeynmanAttemptDetail{}, ErrFeynmanIdempotencyConflict
	}
	now := time.Now()
	detail := &FeynmanAttemptDetail{
		Attempt: FeynmanAttempt{
			AttemptID:        params.AttemptID,
			UserID:           params.UserID,
			KnowledgePointID: params.KnowledgePointID,
			IdempotencyKey:   params.IdempotencyKey,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
	}
	r.attempts[params.AttemptID] = detail
	r.idempotency[params.UserID+"\x00"+params.IdempotencyKey] = params.AttemptID
	return *detail, nil
}

func (r *fakeFeynmanRepository) GetAttemptDetail(_ context.Context, userID, attemptID string) (FeynmanAttemptDetail, error) {
	detail, found := r.attempts[attemptID]
	if !found || detail.Attempt.UserID != userID {
		return FeynmanAttemptDetail{}, ErrFeynmanAttemptNotFound
	}
	return *detail, nil
}

func (r *fakeFeynmanRepository) ClaimAudioTask(_ context.Context, params ClaimFeynmanAudioTaskParams) (FeynmanAttemptDetail, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	detail, found := r.attempts[params.AttemptID]
	if !found || detail.Attempt.UserID != params.UserID {
		return FeynmanAttemptDetail{}, false, ErrFeynmanAttemptNotFound
	}
	if detail.Confirmation != nil {
		return FeynmanAttemptDetail{}, false, ErrFeynmanAttemptConfirmed
	}
	hashKey := params.AttemptID + "\x00" + string(params.SHA256)
	if existingID, exists := r.audioHashes[hashKey]; exists {
		task := r.audioTasks[existingID]
		stale := task.Status == FeynmanAudioStatusTranscribing && task.UpdatedAt.Before(params.StaleBefore)
		if task.Status != FeynmanAudioStatusFailed && !stale {
			return *detail, false, nil
		}
		task.Status = FeynmanAudioStatusTranscribing
		task.TranscriptError = ""
		task.RawTranscript = ""
		task.STTProvider = params.STTProvider
		task.UpdatedAt = time.Now()
		detail.ActiveAudioTask = task
		detail.Attempt.ActiveAudioTaskID = task.AudioTaskID
		return *detail, true, nil
	}
	now := time.Now()
	task := &FeynmanAudioTask{
		AudioTaskID: params.AudioTaskID,
		AttemptID:   params.AttemptID,
		UserID:      params.UserID,
		AttemptNo:   r.nextSeq(),
		Status:      FeynmanAudioStatusTranscribing,
		MIMEType:    params.MIMEType,
		SizeBytes:   params.SizeBytes,
		DurationMs:  params.DurationMs,
		SHA256:      params.SHA256,
		STTProvider: params.STTProvider,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	r.audioTasks[task.AudioTaskID] = task
	r.audioHashes[hashKey] = task.AudioTaskID
	detail.ActiveAudioTask = task
	detail.Attempt.ActiveAudioTaskID = task.AudioTaskID
	detail.Attempt.UpdatedAt = now
	return *detail, true, nil
}

func (r *fakeFeynmanRepository) CompleteAudioTask(_ context.Context, params CompleteFeynmanAudioTaskParams) (FeynmanAttemptDetail, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, found := r.audioTasks[params.AudioTaskID]
	if !found || task.AttemptID != params.AttemptID || task.UserID != params.UserID {
		return FeynmanAttemptDetail{}, ErrFeynmanAttemptNotFound
	}
	if task.Status != FeynmanAudioStatusTranscribing {
		return FeynmanAttemptDetail{}, errors.New("task is not transcribing")
	}
	task.Status = params.Status
	task.STTProvider = params.STTProvider
	task.STTModel = params.STTModel
	task.STTRequestID = params.STTRequestID
	task.RawTranscript = params.RawTranscript
	task.TranscriptError = params.TranscriptError
	task.UpdatedAt = time.Now()
	return *r.attempts[params.AttemptID], nil
}

func (r *fakeFeynmanRepository) ConfirmTranscript(_ context.Context, params ConfirmFeynmanTranscriptParams) (FeynmanAttemptDetail, error) {
	detail, found := r.attempts[params.AttemptID]
	if !found || detail.Attempt.UserID != params.UserID {
		return FeynmanAttemptDetail{}, ErrFeynmanAttemptNotFound
	}
	if detail.Confirmation != nil {
		return FeynmanAttemptDetail{}, ErrFeynmanAttemptConfirmed
	}
	if detail.ActiveAudioTask == nil {
		return FeynmanAttemptDetail{}, ErrFeynmanNoActiveAudio
	}
	if detail.ActiveAudioTask.Status != FeynmanAudioStatusTranscribed {
		return FeynmanAttemptDetail{}, ErrFeynmanAudioNotReady
	}
	confirmation := &FeynmanTranscriptConfirmation{
		ConfirmationID:      params.ConfirmationID,
		AttemptID:           params.AttemptID,
		AudioTaskID:         detail.ActiveAudioTask.AudioTaskID,
		UserID:              params.UserID,
		RawTranscript:       detail.ActiveAudioTask.RawTranscript,
		ConfirmedTranscript: params.ConfirmedTranscript,
		Edited:              detail.ActiveAudioTask.RawTranscript != params.ConfirmedTranscript,
		ConfirmedBy:         params.ConfirmedBy,
		ConfirmedAt:         time.Now(),
	}
	detail.Confirmation = confirmation
	detail.Attempt.ConfirmationID = confirmation.ConfirmationID
	detail.Attempt.UpdatedAt = time.Now()
	return *detail, nil
}

func (r *fakeFeynmanRepository) GetActiveRubric(_ context.Context, userID, knowledgePointID string) (KnowledgePointRubric, bool, error) {
	if owner, exists := r.knowledgePointUser[knowledgePointID]; !exists || owner != userID {
		return KnowledgePointRubric{}, false, nil
	}
	rubricID, found := r.currentRubric[knowledgePointID]
	if !found {
		return KnowledgePointRubric{}, false, nil
	}
	for _, rubric := range r.rubrics[knowledgePointID] {
		if rubric.RubricID == rubricID {
			return rubric, true, nil
		}
	}
	return KnowledgePointRubric{}, false, nil
}

func (r *fakeFeynmanRepository) CreateRubricVersion(_ context.Context, params CreateRubricVersionParams) (KnowledgePointRubric, error) {
	owner, exists := r.knowledgePointUser[params.KnowledgePointID]
	if !exists || owner != params.UserID {
		return KnowledgePointRubric{}, ErrFeynmanKnowledgePointNotFound
	}
	rubric := KnowledgePointRubric{
		RubricID:         params.RubricID,
		KnowledgePointID: params.KnowledgePointID,
		UserID:           params.UserID,
		VersionNo:        len(r.rubrics[params.KnowledgePointID]) + 1,
		TemplateVersion:  params.TemplateVersion,
		Criteria:         params.Criteria,
		CreatedBy:        params.CreatedBy,
		CreatedAt:        time.Now(),
	}
	r.rubrics[params.KnowledgePointID] = append(r.rubrics[params.KnowledgePointID], rubric)
	r.currentRubric[params.KnowledgePointID] = rubric.RubricID
	return rubric, nil
}

func (r *fakeFeynmanRepository) InitializeRubric(ctx context.Context, params CreateRubricVersionParams) (KnowledgePointRubric, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if currentID := r.currentRubric[params.KnowledgePointID]; currentID != "" {
		for _, rubric := range r.rubrics[params.KnowledgePointID] {
			if rubric.RubricID == currentID {
				return rubric, nil
			}
		}
	}
	return r.createRubricVersionUnlocked(ctx, params)
}

func (r *fakeFeynmanRepository) createRubricVersionUnlocked(_ context.Context, params CreateRubricVersionParams) (KnowledgePointRubric, error) {
	owner, exists := r.knowledgePointUser[params.KnowledgePointID]
	if !exists || owner != params.UserID {
		return KnowledgePointRubric{}, ErrFeynmanKnowledgePointNotFound
	}
	rubric := KnowledgePointRubric{
		RubricID: params.RubricID, KnowledgePointID: params.KnowledgePointID, UserID: params.UserID,
		VersionNo: len(r.rubrics[params.KnowledgePointID]) + 1, TemplateVersion: params.TemplateVersion,
		Criteria: params.Criteria, CreatedBy: params.CreatedBy, CreatedAt: time.Now(),
	}
	r.rubrics[params.KnowledgePointID] = append(r.rubrics[params.KnowledgePointID], rubric)
	r.currentRubric[params.KnowledgePointID] = rubric.RubricID
	return rubric, nil
}

// ---------------------------------------------------------------------------
// 假 STT 供应商
// ---------------------------------------------------------------------------

type fakeSTTProvider struct {
	mu      sync.Mutex
	name    string
	text    string
	err     error
	calls   int
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (p *fakeSTTProvider) Name() string { return p.name }

func (p *fakeSTTProvider) Transcribe(_ context.Context, _ []byte, _ string) (stt.Transcript, error) {
	p.mu.Lock()
	p.calls++
	providerErr := p.err
	text := p.text
	p.mu.Unlock()
	if p.started != nil {
		p.once.Do(func() { close(p.started) })
	}
	if p.release != nil {
		<-p.release
	}
	if providerErr != nil {
		return stt.Transcript{}, providerErr
	}
	return stt.Transcript{Text: text, Provider: p.name, Model: "fake-model", RequestID: "req-1"}, nil
}

func (p *fakeSTTProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type controlledSTTProvider struct {
	mu      sync.Mutex
	started map[string]chan struct{}
	release map[string]chan struct{}
	calls   map[string]int
}

func newControlledSTTProvider() *controlledSTTProvider {
	return &controlledSTTProvider{
		started: map[string]chan struct{}{"audio-a": make(chan struct{}), "audio-b": make(chan struct{})},
		release: map[string]chan struct{}{"audio-a": make(chan struct{}), "audio-b": make(chan struct{})},
		calls:   map[string]int{},
	}
}

func (p *controlledSTTProvider) Name() string { return "controlled" }

func (p *controlledSTTProvider) Transcribe(_ context.Context, audio []byte, _ string) (stt.Transcript, error) {
	key := string(audio)
	p.mu.Lock()
	p.calls[key]++
	started := p.started[key]
	release := p.release[key]
	p.mu.Unlock()
	close(started)
	<-release
	return stt.Transcript{Text: "transcript-" + key, Provider: p.Name(), Model: "fake-model"}, nil
}

// ---------------------------------------------------------------------------
// 测试
// ---------------------------------------------------------------------------

func newTestFeynmanService(repo FeynmanRepository, provider stt.Provider) *FeynmanService {
	return NewFeynmanService(repo, provider, FeynmanLimits{
		MaxAudioBytes:      1024,
		MaxDurationMS:      180000,
		MaxTranscriptChars: 200,
	}, nil)
}

func TestFeynmanCreateAttemptIsIdempotent(t *testing.T) {
	repo := newFakeFeynmanRepository()
	repo.knowledgePointUser["kp-1"] = "user-1"
	svc := newTestFeynmanService(repo, &fakeSTTProvider{name: "fake"})

	first, err := svc.CreateAttempt(context.Background(), "user-1", "kp-1", "idem-1")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := svc.CreateAttempt(context.Background(), "user-1", "kp-1", "idem-1")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if first.Attempt.AttemptID != second.Attempt.AttemptID {
		t.Fatalf("expected same attempt id, got %q vs %q", first.Attempt.AttemptID, second.Attempt.AttemptID)
	}
}

func TestFeynmanCreateAttemptRejectsIdempotencyKeyForDifferentTopic(t *testing.T) {
	repo := newFakeFeynmanRepository()
	repo.knowledgePointUser["kp-1"] = "user-1"
	repo.knowledgePointUser["kp-2"] = "user-1"
	svc := newTestFeynmanService(repo, &fakeSTTProvider{name: "fake"})

	if _, err := svc.CreateAttempt(context.Background(), "user-1", "kp-1", "idem-1"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := svc.CreateAttempt(context.Background(), "user-1", "kp-2", "idem-1"); !errors.Is(err, ErrFeynmanIdempotencyMismatch) {
		t.Fatalf("expected ErrFeynmanIdempotencyMismatch, got %v", err)
	}
}

func TestFeynmanCreateAttemptRejectsUnknownKnowledgePoint(t *testing.T) {
	repo := newFakeFeynmanRepository()
	svc := newTestFeynmanService(repo, &fakeSTTProvider{name: "fake"})

	_, err := svc.CreateAttempt(context.Background(), "user-1", "kp-missing", "idem-1")
	if !errors.Is(err, ErrFeynmanKnowledgePointNotFound) {
		t.Fatalf("expected ErrFeynmanKnowledgePointNotFound, got %v", err)
	}
}

func TestFeynmanUploadAudioTranscribesAndActivates(t *testing.T) {
	repo := newFakeFeynmanRepository()
	repo.knowledgePointUser["kp-1"] = "user-1"
	provider := &fakeSTTProvider{name: "fake", text: "hello world"}
	svc := newTestFeynmanService(repo, provider)

	attempt, err := svc.CreateAttempt(context.Background(), "user-1", "kp-1", "idem-1")
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}

	detail, err := svc.UploadAudio(context.Background(), "user-1", attempt.Attempt.AttemptID, []byte("audio-bytes"), "audio/webm", nil)
	if err != nil {
		t.Fatalf("upload audio: %v", err)
	}
	if detail.ActiveAudioTask == nil {
		t.Fatalf("expected active audio task")
	}
	if detail.ActiveAudioTask.Status != FeynmanAudioStatusTranscribed {
		t.Fatalf("status = %q, want transcribed", detail.ActiveAudioTask.Status)
	}
	if detail.ActiveAudioTask.RawTranscript != "hello world" {
		t.Fatalf("raw transcript = %q, want %q", detail.ActiveAudioTask.RawTranscript, "hello world")
	}
	if detail.Status() != FeynmanAttemptStatusTranscribed {
		t.Fatalf("attempt status = %q, want transcribed", detail.Status())
	}

	// 重复提交完全相同的字节应命中去重，不再调用 STT。
	if _, err := svc.UploadAudio(context.Background(), "user-1", attempt.Attempt.AttemptID, []byte("audio-bytes"), "audio/webm", nil); err != nil {
		t.Fatalf("dedupe upload: %v", err)
	}
	if provider.callCount() != 1 {
		t.Fatalf("stt calls = %d, want 1 (dedupe should skip re-transcription)", provider.callCount())
	}
}

func TestFeynmanConcurrentDuplicateUploadCallsSTTOnce(t *testing.T) {
	repo := newFakeFeynmanRepository()
	repo.knowledgePointUser["kp-1"] = "user-1"
	started := make(chan struct{})
	release := make(chan struct{})
	provider := &fakeSTTProvider{name: "fake", text: "hello", started: started, release: release}
	svc := newTestFeynmanService(repo, provider)
	attempt, err := svc.CreateAttempt(context.Background(), "user-1", "kp-1", "idem-1")
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, uploadErr := svc.UploadAudio(context.Background(), "user-1", attempt.Attempt.AttemptID, []byte("same-audio"), "audio/webm", nil)
		firstDone <- uploadErr
	}()
	<-started

	second, err := svc.UploadAudio(context.Background(), "user-1", attempt.Attempt.AttemptID, []byte("same-audio"), "audio/webm", nil)
	if err != nil {
		t.Fatalf("duplicate upload: %v", err)
	}
	if second.Status() != FeynmanAttemptStatusTranscribing {
		t.Fatalf("duplicate status = %q, want transcribing", second.Status())
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if provider.callCount() != 1 {
		t.Fatalf("stt calls = %d, want 1", provider.callCount())
	}
}

func TestFeynmanLaterRecordingStaysActiveWhenEarlierSTTFinishesLast(t *testing.T) {
	repo := newFakeFeynmanRepository()
	repo.knowledgePointUser["kp-1"] = "user-1"
	provider := newControlledSTTProvider()
	svc := newTestFeynmanService(repo, provider)
	attempt, err := svc.CreateAttempt(context.Background(), "user-1", "kp-1", "idem-1")
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, uploadErr := svc.UploadAudio(context.Background(), "user-1", attempt.Attempt.AttemptID, []byte("audio-a"), "audio/webm", nil)
		firstDone <- uploadErr
	}()
	<-provider.started["audio-a"]

	secondDone := make(chan error, 1)
	go func() {
		_, uploadErr := svc.UploadAudio(context.Background(), "user-1", attempt.Attempt.AttemptID, []byte("audio-b"), "audio/webm", nil)
		secondDone <- uploadErr
	}()
	<-provider.started["audio-b"]
	close(provider.release["audio-b"])
	if err := <-secondDone; err != nil {
		t.Fatalf("second upload: %v", err)
	}
	close(provider.release["audio-a"])
	if err := <-firstDone; err != nil {
		t.Fatalf("first upload: %v", err)
	}

	detail, err := svc.GetAttempt(context.Background(), "user-1", attempt.Attempt.AttemptID)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if detail.ActiveAudioTask == nil || detail.ActiveAudioTask.RawTranscript != "transcript-audio-b" {
		t.Fatalf("active task = %+v, want later recording audio-b", detail.ActiveAudioTask)
	}
}

func TestFeynmanUploadAudioRejectsOversizedAndBadMIME(t *testing.T) {
	repo := newFakeFeynmanRepository()
	repo.knowledgePointUser["kp-1"] = "user-1"
	svc := newTestFeynmanService(repo, &fakeSTTProvider{name: "fake"})
	attempt, err := svc.CreateAttempt(context.Background(), "user-1", "kp-1", "idem-1")
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}

	oversized := make([]byte, svc.Limits().MaxAudioBytes+1)
	if _, err := svc.UploadAudio(context.Background(), "user-1", attempt.Attempt.AttemptID, oversized, "audio/webm", nil); !isFeynmanInputError(err) {
		t.Fatalf("expected input error for oversized audio, got %v", err)
	}

	if _, err := svc.UploadAudio(context.Background(), "user-1", attempt.Attempt.AttemptID, []byte("bytes"), "video/mp4", nil); !isFeynmanInputError(err) {
		t.Fatalf("expected input error for unsupported mime, got %v", err)
	}
}

func TestFeynmanUploadAudioRecordsFailureWithoutBlockingRetry(t *testing.T) {
	repo := newFakeFeynmanRepository()
	repo.knowledgePointUser["kp-1"] = "user-1"
	provider := &fakeSTTProvider{name: "fake", err: errors.New("upstream unavailable")}
	svc := newTestFeynmanService(repo, provider)
	attempt, err := svc.CreateAttempt(context.Background(), "user-1", "kp-1", "idem-1")
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}

	detail, err := svc.UploadAudio(context.Background(), "user-1", attempt.Attempt.AttemptID, []byte("audio-bytes"), "audio/webm", nil)
	if err != nil {
		t.Fatalf("upload audio: %v", err)
	}
	if detail.ActiveAudioTask.Status != FeynmanAudioStatusFailed {
		t.Fatalf("status = %q, want failed", detail.ActiveAudioTask.Status)
	}
	if detail.Status() != FeynmanAttemptStatusFailed {
		t.Fatalf("attempt status = %q, want failed", detail.Status())
	}

	// 失败后允许换一段新字节重新上传（重录）。
	provider.err = nil
	provider.text = "second try"
	detail, err = svc.UploadAudio(context.Background(), "user-1", attempt.Attempt.AttemptID, []byte("other-bytes"), "audio/webm", nil)
	if err != nil {
		t.Fatalf("retry upload: %v", err)
	}
	if detail.ActiveAudioTask.Status != FeynmanAudioStatusTranscribed {
		t.Fatalf("status = %q, want transcribed after retry", detail.ActiveAudioTask.Status)
	}
}

func TestFeynmanConfirmTranscriptRequiresTranscribedAudio(t *testing.T) {
	repo := newFakeFeynmanRepository()
	repo.knowledgePointUser["kp-1"] = "user-1"
	svc := newTestFeynmanService(repo, &fakeSTTProvider{name: "fake"})
	attempt, err := svc.CreateAttempt(context.Background(), "user-1", "kp-1", "idem-1")
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}

	if _, err := svc.ConfirmTranscript(context.Background(), "user-1", attempt.Attempt.AttemptID, "confirmed"); !errors.Is(err, ErrFeynmanNoActiveAudio) {
		t.Fatalf("expected ErrFeynmanNoActiveAudio, got %v", err)
	}
}

func TestFeynmanConfirmTranscriptLocksAttempt(t *testing.T) {
	repo := newFakeFeynmanRepository()
	repo.knowledgePointUser["kp-1"] = "user-1"
	provider := &fakeSTTProvider{name: "fake", text: "raw text"}
	svc := newTestFeynmanService(repo, provider)
	attempt, err := svc.CreateAttempt(context.Background(), "user-1", "kp-1", "idem-1")
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if _, err := svc.UploadAudio(context.Background(), "user-1", attempt.Attempt.AttemptID, []byte("audio-bytes"), "audio/webm", nil); err != nil {
		t.Fatalf("upload audio: %v", err)
	}

	detail, err := svc.ConfirmTranscript(context.Background(), "user-1", attempt.Attempt.AttemptID, "corrected text")
	if err != nil {
		t.Fatalf("confirm transcript: %v", err)
	}
	if detail.Confirmation == nil {
		t.Fatalf("expected confirmation to be set")
	}
	if !detail.Confirmation.Edited {
		t.Fatalf("expected edited=true since confirmed text differs from raw")
	}
	if detail.Status() != FeynmanAttemptStatusTranscriptConfirmed {
		t.Fatalf("attempt status = %q, want transcript_confirmed", detail.Status())
	}

	// 已确认的练习永久只读，不能再上传录音或再次确认。
	if _, err := svc.UploadAudio(context.Background(), "user-1", attempt.Attempt.AttemptID, []byte("more-bytes"), "audio/webm", nil); !errors.Is(err, ErrFeynmanAttemptConfirmed) {
		t.Fatalf("expected ErrFeynmanAttemptConfirmed on upload, got %v", err)
	}
	if _, err := svc.ConfirmTranscript(context.Background(), "user-1", attempt.Attempt.AttemptID, "again"); !errors.Is(err, ErrFeynmanAttemptConfirmed) {
		t.Fatalf("expected ErrFeynmanAttemptConfirmed on re-confirm, got %v", err)
	}
}

func TestFeynmanGetRubricAutoCreatesTemplateV1(t *testing.T) {
	repo := newFakeFeynmanRepository()
	repo.knowledgePointUser["kp-1"] = "user-1"
	svc := newTestFeynmanService(repo, &fakeSTTProvider{name: "fake"})

	rubric, err := svc.GetRubric(context.Background(), "user-1", "kp-1")
	if err != nil {
		t.Fatalf("get rubric: %v", err)
	}
	if rubric.VersionNo != 1 || rubric.TemplateVersion != FeynmanRubricTemplateV1 {
		t.Fatalf("unexpected rubric version: %+v", rubric)
	}
	if len(rubric.Criteria) != len(requiredRubricDimensions) {
		t.Fatalf("criteria count = %d, want %d", len(rubric.Criteria), len(requiredRubricDimensions))
	}

	// 第二次访问复用已有版本，不新建。
	again, err := svc.GetRubric(context.Background(), "user-1", "kp-1")
	if err != nil {
		t.Fatalf("get rubric second time: %v", err)
	}
	if again.RubricID != rubric.RubricID {
		t.Fatalf("expected same rubric id on repeated GetRubric")
	}
}

func TestFeynmanCreateRubricVersionValidatesCriteria(t *testing.T) {
	repo := newFakeFeynmanRepository()
	repo.knowledgePointUser["kp-1"] = "user-1"
	svc := newTestFeynmanService(repo, &fakeSTTProvider{name: "fake"})

	badWeights := DefaultRubricCriteria()
	badWeights[0].Weight = 50 // 总和不再是 100
	if _, err := svc.CreateRubricVersion(context.Background(), "user-1", "kp-1", badWeights); !isFeynmanInputError(err) {
		t.Fatalf("expected input error for bad weight sum, got %v", err)
	}

	missingDimension := DefaultRubricCriteria()[:4] // 缺一个维度
	if _, err := svc.CreateRubricVersion(context.Background(), "user-1", "kp-1", missingDimension); !isFeynmanInputError(err) {
		t.Fatalf("expected input error for missing dimension, got %v", err)
	}

	valid := DefaultRubricCriteria()
	rubric, err := svc.CreateRubricVersion(context.Background(), "user-1", "kp-1", valid)
	if err != nil {
		t.Fatalf("create valid rubric version: %v", err)
	}
	if rubric.VersionNo != 1 {
		t.Fatalf("version_no = %d, want 1", rubric.VersionNo)
	}

	second, err := svc.CreateRubricVersion(context.Background(), "user-1", "kp-1", valid)
	if err != nil {
		t.Fatalf("create second rubric version: %v", err)
	}
	if second.VersionNo != 2 {
		t.Fatalf("version_no = %d, want 2", second.VersionNo)
	}
}

func isFeynmanInputError(err error) bool {
	var inputErr *FeynmanInputError
	return errors.As(err, &inputErr)
}
