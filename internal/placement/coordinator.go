package placement

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrConflict           = errors.New("placement ledger revision conflict")
	ErrLedgerUnavailable  = errors.New("placement ledger unavailable")
	ErrLedgerFull         = errors.New("placement ledger limit reached")
	ErrIdentityConflict   = errors.New("machine identity or VMID already claimed with different intent")
	ErrPolicyConflict     = errors.New("placement group policy differs from its durable contract")
	ErrUncertainOperation = errors.New("clone submission already claimed; reconciliation required")
)

// These are defensive implementation bounds, not qualified production scale.
const (
	MaxLedgerRecords = 256
	MaxLedgerBytes   = 512 * 1024
	maxCASAttempts   = 64
)

type Phase string

const (
	Reserved   Phase = "reserved"
	Submitting Phase = "submitting"
	Submitted  Phase = "submitted"
)

// Record never expires. Submitting includes the ambiguous "accepted but response
// lost" case. A submission token authorizes one caller, not a Proxmox fencing token.
type Record struct {
	Commitment
	PolicyHash      string    `json:"policyHash"`
	Phase           Phase     `json:"phase"`
	SubmissionToken string    `json:"submissionToken,omitempty"`
	TaskID          string    `json:"taskId,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
}

// Ledger is the single-object transaction boundary for one Proxmox cluster.
// It contains no API credentials, private keys or cloud-init secrets.
type Ledger struct {
	Version        int               `json:"version"`
	ClusterID      string            `json:"clusterId"`
	CapacityPolicy CapacityPolicy    `json:"capacityPolicy"`
	Groups         map[string]string `json:"groups"`
	Records        map[string]Record `json:"records"`
}

// Document couples durable state with an opaque revision and object incarnation.
type Document struct {
	UID      string
	Revision string
	Ledger   Ledger
	// envelope preserves unrelated Kubernetes metadata during updates.
	envelope map[string]json.RawMessage
}

// Store must implement atomic compare-and-swap; a process-local mutex is not a
// production implementation. CompareAndSwap must also preserve object UID.
type Store interface {
	Read(ctx context.Context) (Document, error)
	CompareAndSwap(ctx context.Context, previous Document, next Ledger) error
}

// Coordinator never assumes that a Store error means a write did not commit.
// Production stores must use durable shared storage, such as KubernetesStore.
type Coordinator struct {
	Store     Store
	ClusterID string
}

type Intent struct {
	MachineID string
	OwnerID   string
	VMID      int
	Policy    Policy
	Resources Resources
}

// Discover is called again after every CAS conflict. It must obtain complete,
// fresh evidence; reusing a cached pre-reservation inventory is not safe.
type Discover func(context.Context) (Snapshot, error)

// Reserve atomically claims VMID, resources and group placement. Replaying the
// same intent returns the original record, never a second placement. The caller
// must still call BeginSubmission and receive its unique authorization to clone.
//
//nolint:gocognit,gocyclo,cyclop // The safety-critical reservation transaction is kept together.
func (c Coordinator) Reserve(ctx context.Context, intent Intent, discover Discover) (Record, Decision, error) {
	if c.Store == nil || !validIdentity(c.ClusterID) || discover == nil ||
		!validIdentity(intent.MachineID) || !validIdentity(intent.OwnerID) || intent.VMID < 100 || intent.VMID > 999999999 ||
		intent.Resources.CPU == 0 || intent.Resources.MemoryBytes == 0 || intent.Resources.DiskBytes == 0 {
		return Record{}, Decision{}, errors.New("invalid coordinator or machine intent")
	}

	fingerprint, err := intent.Policy.Fingerprint()
	if err != nil {
		return Record{}, Decision{}, err
	}

	for range maxCASAttempts {
		document, err := c.read(ctx)
		if err != nil {
			return Record{}, Decision{}, err
		}

		ledger := document.Ledger
		if hash, found := ledger.Groups[intent.Policy.GroupID]; found && hash != fingerprint {
			return Record{}, Decision{}, ErrPolicyConflict
		}

		if record, found := ledger.Records[intent.MachineID]; found {
			if record.OwnerID != intent.OwnerID || record.VMID != intent.VMID || record.PolicyHash != fingerprint || record.Resources != intent.Resources {
				return Record{}, Decision{}, ErrIdentityConflict
			}

			return record, Decision{}, nil
		}

		commitments := make([]Commitment, 0, len(ledger.Records))

		for _, record := range ledger.Records {
			if record.VMID == intent.VMID || record.OwnerID == intent.OwnerID {
				return Record{}, Decision{}, ErrIdentityConflict
			}

			commitments = append(commitments, record.Commitment)
		}

		if len(ledger.Records) >= MaxLedgerRecords {
			return Record{}, Decision{}, ErrLedgerFull
		}

		inventory, err := discover(ctx)
		if err != nil {
			return Record{}, Decision{}, fmt.Errorf("discover placement evidence: %w", err)
		}
		// Capacity policy is authoritative in the same atomic ledger as the
		// reservation; discovery cannot supply or weaken administrative reserves.
		inventory.CapacityPolicy = ledger.CapacityPolicy
		// An observed identity without its durable record is not automatically
		// adoptable (for example after coordinator backup restoration).
		for _, machine := range inventory.Machines {
			if machine.MachineID == intent.MachineID || machine.OwnerID == intent.OwnerID || machine.VMID == intent.VMID {
				return Record{}, Decision{}, ErrIdentityConflict
			}
		}

		decision, err := Select(ctx, intent.Policy, intent.Resources, inventory, commitments)
		if err != nil {
			return Record{}, decision, fmt.Errorf("select destination: %w", err)
		}

		record := Record{
			Commitment: Commitment{
				MachineID: intent.MachineID, OwnerID: intent.OwnerID, GroupID: intent.Policy.GroupID,
				VMID: intent.VMID, Node: decision.Selected.Node, Storage: decision.Selected.Storage, Resources: intent.Resources,
			},
			PolicyHash: fingerprint, Phase: Reserved, CreatedAt: time.Now().UTC(),
		}
		ledger.Groups[intent.Policy.GroupID] = fingerprint
		ledger.Records[intent.MachineID] = record

		if err := ledger.Validate(); err != nil {
			return Record{}, decision, fmt.Errorf("commit reservation: %w", err)
		}

		if err := c.Store.CompareAndSwap(ctx, document, ledger); err != nil {
			if errors.Is(err, ErrConflict) {
				continue
			}

			return Record{}, decision, fmt.Errorf("commit reservation: %w", err)
		}

		return record, decision, nil
	}

	return Record{}, Decision{}, fmt.Errorf("reservation contention exceeded bounded retries: %w", ErrConflict)
}

// BeginSubmission grants exactly one successful caller a token to submit a clone.
// If the write response is lost, this returns an error and the caller MUST NOT
// clone. Even then the record remains occupied for explicit reconciliation.
func (c Coordinator) BeginSubmission(ctx context.Context, machineID, ownerID string) (Record, error) {
	return c.change(ctx, machineID, ownerID, func(record Record) (Record, error) {
		if record.Phase != Reserved {
			return Record{}, ErrUncertainOperation
		}

		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return Record{}, fmt.Errorf("generate submission identity: %w", err)
		}

		record.Phase, record.SubmissionToken = Submitting, hex.EncodeToString(token[:])

		return record, nil
	})
}

// RecordTask durably records a returned UPID. It does not release the commitment,
// establish task completion, or authorize another submission.
func (c Coordinator) RecordTask(ctx context.Context, machineID, ownerID, token, taskID string) (Record, error) {
	if token == "" || taskID == "" || len(taskID) > 512 {
		return Record{}, errors.New("submission token and bounded task ID are required")
	}

	return c.change(ctx, machineID, ownerID, func(record Record) (Record, error) {
		if record.SubmissionToken != token {
			return Record{}, ErrIdentityConflict
		}

		if record.Phase == Submitted && record.TaskID == taskID {
			return record, nil
		}

		if record.Phase != Submitting {
			return Record{}, ErrUncertainOperation
		}

		record.Phase, record.TaskID = Submitted, taskID

		return record, nil
	})
}

func (c Coordinator) read(ctx context.Context) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, fmt.Errorf("read ledger context: %w", err)
	}

	if c.Store == nil || !validIdentity(c.ClusterID) {
		return Document{}, ErrLedgerUnavailable
	}

	document, err := c.Store.Read(ctx)
	if err != nil {
		return Document{}, fmt.Errorf("read ledger: %w", err)
	}

	if document.UID == "" || document.Revision == "" || document.Ledger.ClusterID != c.ClusterID {
		return Document{}, ErrLedgerUnavailable
	}

	if err := document.Ledger.Validate(); err != nil {
		return Document{}, err
	}

	return document, nil
}

func (c Coordinator) change(ctx context.Context, machineID, ownerID string, update func(Record) (Record, error)) (Record, error) {
	for range maxCASAttempts {
		document, err := c.read(ctx)
		if err != nil {
			return Record{}, fmt.Errorf("commit operation: %w", err)
		}

		record, found := document.Ledger.Records[machineID]
		if !found || record.OwnerID != ownerID {
			return Record{}, ErrIdentityConflict
		}

		next, err := update(record)
		if err != nil {
			return Record{}, fmt.Errorf("commit operation: %w", err)
		}

		if next == record {
			return record, nil
		}

		ledger := document.Ledger
		ledger.Records[machineID] = next

		if err := ledger.Validate(); err != nil {
			return Record{}, err
		}

		if err := c.Store.CompareAndSwap(ctx, document, ledger); err != nil {
			if errors.Is(err, ErrConflict) {
				continue
			}

			return Record{}, fmt.Errorf("commit operation: %w", err)
		}

		return next, nil
	}

	return Record{}, fmt.Errorf("operation contention exceeded bounded retries: %w", ErrConflict)
}

// Validate fails closed on unknown schema, inconsistent records or size limits.
//
//nolint:gocyclo,cyclop // Explicit validation of durable records fails closed.
func (l Ledger) Validate() error {
	if l.Version != 1 || !validIdentity(l.ClusterID) || l.Groups == nil || l.Records == nil {
		return ErrLedgerUnavailable
	}

	if len(l.Records) > MaxLedgerRecords || len(l.Groups) > MaxLedgerRecords {
		return ErrLedgerFull
	}

	if err := l.CapacityPolicy.Validate(); err != nil {
		return fmt.Errorf("validate central capacity policy: %w", err)
	}

	vmids, owners := make(map[int]bool), make(map[string]bool)
	for id, record := range l.Records {
		if id != record.MachineID || !validIdentity(id) || !validIdentity(record.OwnerID) || !validIdentity(record.GroupID) ||
			!validHost(record.Node) || !storageID.MatchString(record.Storage) || record.VMID < 100 || record.VMID > 999999999 ||
			vmids[record.VMID] || owners[record.OwnerID] || record.CreatedAt.IsZero() ||
			record.Resources.CPU == 0 || record.Resources.MemoryBytes == 0 || record.Resources.DiskBytes == 0 ||
			l.Groups[record.GroupID] != record.PolicyHash || !validHash(record.PolicyHash) {
			return ErrLedgerUnavailable
		}

		vmids[record.VMID], owners[record.OwnerID] = true, true

		switch record.Phase {
		case Reserved:
			if record.SubmissionToken != "" || record.TaskID != "" {
				return ErrLedgerUnavailable
			}
		case Submitting, Submitted:
			if len(record.SubmissionToken) != 32 || len(record.TaskID) > 512 ||
				(record.Phase == Submitting && record.TaskID != "") || (record.Phase == Submitted && record.TaskID == "") {
				return ErrLedgerUnavailable
			}
		default:
			return ErrLedgerUnavailable
		}
	}

	for group, hash := range l.Groups {
		if !validIdentity(group) || !validHash(hash) {
			return ErrLedgerUnavailable
		}
	}

	encoded, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("encode placement ledger: %w", err)
	}

	if len(encoded) > MaxLedgerBytes {
		return ErrLedgerFull
	}

	return nil
}

func validHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}
