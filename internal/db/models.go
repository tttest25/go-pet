package db

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusNew       Status = "new"
	StatusProcessed Status = "processed"
	StatusError     Status = "error"

	StatusPending    Status = "pending"
	StatusDispatched Status = "dispatched"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
)

type Input struct {
	ID        uuid.UUID
	Source    string
	Payload   json.RawMessage
	Status    Status
	CreatedAt time.Time
}

type Job struct {
	ID        uuid.UUID
	ProcessID uuid.UUID
	ToolName  string
	Payload   json.RawMessage
	OldStatus Status
	Status    Status
}
