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

type JobDetails struct {
	ID         uuid.UUID
	ProcessID  uuid.UUID
	ToolName   string
	Payload    json.RawMessage
	Status     Status
	Priority   int
	ResultPath *string
	ErrorMsg   *string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type OutputDetails struct {
	ID           uuid.UUID
	ProcessID    uuid.UUID
	ArtifactType string
	Content      *string
	Metadata     json.RawMessage
	CreatedAt    time.Time
}

type ProcessDetails struct {
	ID        uuid.UUID
	InputID   uuid.UUID
	State     string
	Context   json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
	Jobs      []JobDetails
	Outputs   []OutputDetails
}

type JobFilter struct {
	ProcessID *uuid.UUID
	Status    *Status
	Limit     int
	CursorID  *uuid.UUID
}
