package queue

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

const capacityAdvisoryLock int64 = 0x41525155455545

type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type WithinTxFunc func(context.Context, *sql.TxOptions, func(context.Context, DBTX) error) error

type Config struct {
	MaxPendingPerTenant int
	MaxPendingGlobal    int
}

func DefaultConfig() Config { return Config{MaxPendingPerTenant: 250, MaxPendingGlobal: 6000} }

type PostgresAdapter struct {
	withinTx WithinTxFunc
	config   Config
}

func NewPostgresAdapter(withinTx WithinTxFunc, configuration Config) (*PostgresAdapter, error) {
	if withinTx == nil || configuration.MaxPendingPerTenant < 1 || configuration.MaxPendingPerTenant > 100_000 ||
		configuration.MaxPendingGlobal < configuration.MaxPendingPerTenant || configuration.MaxPendingGlobal > 1_000_000 {
		return nil, fmt.Errorf("%w: PostgreSQL adapter configuration", ErrInvalidArgument)
	}
	return &PostgresAdapter{withinTx: withinTx, config: configuration}, nil
}

func (a *PostgresAdapter) Enqueue(ctx context.Context, message Message) error {
	normalized, err := normalizeMessage(message)
	if err != nil {
		return err
	}
	return a.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, capacityAdvisoryLock); err != nil {
			return fmt.Errorf("lock queue capacity: %w", err)
		}
		existing, err := selectIdentity(ctx, tx, normalized.TenantID, normalized.RunID, normalized.DeliveryID)
		if err == nil {
			if sameMessage(existing, normalized) {
				return nil
			}
			return ErrConflict
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		var tenantPending, globalPending int
		if err := tx.QueryRowContext(ctx, pendingCountsSQL, normalized.TenantID.String()).Scan(&tenantPending, &globalPending); err != nil {
			return fmt.Errorf("read queue capacity: %w", err)
		}
		if tenantPending >= a.config.MaxPendingPerTenant {
			return ErrTenantCapacity
		}
		if globalPending >= a.config.MaxPendingGlobal {
			return ErrGlobalCapacity
		}
		_, err = tx.ExecContext(ctx, insertMessageSQL, normalized.TenantID.String(), normalized.RunID.String(),
			normalized.SessionID.String(), normalized.TurnID.String(), normalized.DeliveryID, string(normalized.Payload),
			nullableString(normalized.Trace.TraceParent), nullableString(normalized.Trace.TraceState),
			normalized.AvailableAt, normalized.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert queue message: %w", err)
		}
		return nil
	})
}

func (a *PostgresAdapter) Claim(ctx context.Context, command ClaimCommand) ([]Delivery, error) {
	if err := validateClaim(command); err != nil {
		return nil, err
	}
	var result []Delivery
	err := a.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		rows, err := tx.QueryContext(ctx, claimSQL, command.TenantID.String(), command.Owner, command.At.UTC(),
			command.Duration.Milliseconds(), command.Limit)
		if err != nil {
			return fmt.Errorf("claim queue messages: %w", err)
		}
		defer rows.Close()
		result = make([]Delivery, 0, command.Limit)
		for rows.Next() {
			delivery, err := scanDelivery(rows)
			if err != nil {
				return err
			}
			result = append(result, delivery)
		}
		return rows.Err()
	})
	return result, err
}

func (a *PostgresAdapter) Renew(ctx context.Context, command RenewCommand) (Delivery, error) {
	if err := validateLease(command.LeaseCommand); err != nil || command.Duration < MinimumLeaseDuration || command.Duration > MaximumLeaseDuration {
		if err != nil {
			return Delivery{}, err
		}
		return Delivery{}, fmt.Errorf("%w: renewal duration", ErrInvalidArgument)
	}
	var delivery Delivery
	err := a.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		var err error
		delivery, err = scanDelivery(tx.QueryRowContext(ctx, renewSQL, command.TenantID.String(), command.RunID.String(),
			command.Owner, command.Token, command.At.UTC(), command.Duration.Milliseconds()))
		if errors.Is(err, sql.ErrNoRows) {
			return classifyLease(ctx, tx, command.LeaseCommand)
		}
		return err
	})
	return delivery, err
}

func (a *PostgresAdapter) Ack(ctx context.Context, command LeaseCommand) error {
	if err := validateLease(command); err != nil {
		return err
	}
	return a.mutateLease(ctx, command, ackSQL, command.At.UTC())
}

func (a *PostgresAdapter) Nack(ctx context.Context, command NackCommand) error {
	if err := validateLease(command.LeaseCommand); err != nil {
		return err
	}
	if command.RetryAfter < 0 || command.RetryAfter > 24*time.Hour || len(command.Reason) < 1 ||
		len(command.Reason) > MaximumErrorLength || !validSafeText(command.Reason, MaximumErrorLength) {
		return fmt.Errorf("%w: Nack retry or safe reason", ErrInvalidArgument)
	}
	return a.mutateLease(ctx, command.LeaseCommand, nackSQL, command.At.UTC(), command.RetryAfter.Milliseconds(), command.Reason)
}

func (a *PostgresAdapter) mutateLease(ctx context.Context, command LeaseCommand, query string, arguments ...any) error {
	return a.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		values := []any{command.TenantID.String(), command.RunID.String(), command.Owner, command.Token}
		values = append(values, arguments...)
		result, err := tx.ExecContext(ctx, query, values...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 1 {
			return nil
		}
		return classifyLease(ctx, tx, command)
	})
}

func (a *PostgresAdapter) Cleanup(ctx context.Context, command CleanupCommand) (int64, error) {
	if _, err := domain.ParseTenantID(command.TenantID.String()); err != nil || command.Cutoff.IsZero() || command.Limit < 1 || command.Limit > 1000 {
		return 0, fmt.Errorf("%w: cleanup tenant, cutoff or limit", ErrInvalidArgument)
	}
	var count int64
	err := a.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		result, err := tx.ExecContext(ctx, cleanupSQL, command.TenantID.String(), command.Cutoff.UTC(), command.Limit)
		if err != nil {
			return err
		}
		count, err = result.RowsAffected()
		return err
	})
	return count, err
}

func selectIdentity(ctx context.Context, tx DBTX, tenantID domain.TenantID, runID domain.RunID, deliveryID string) (Message, error) {
	message, err := scanMessage(tx.QueryRowContext(ctx, selectIdentitySQL, tenantID.String(), runID.String(), deliveryID))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, err
	}
	return normalizeMessage(message)
}

func classifyLease(ctx context.Context, tx DBTX, command LeaseCommand) error {
	var status string
	var owner sql.NullString
	var token uint64
	var expires sql.NullTime
	err := tx.QueryRowContext(ctx, selectLeaseSQL, command.TenantID.String(), command.RunID.String()).Scan(&status, &owner, &token, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == "LEASED" && owner.Valid && owner.String == command.Owner && token == command.Token && expires.Valid && !expires.Time.After(command.At.UTC()) {
		return ErrLeaseExpired
	}
	return ErrFenced
}

type rowScanner interface{ Scan(...any) error }

func scanMessage(scanner rowScanner) (Message, error) {
	var message Message
	var tenant, session, turn, run string
	var traceParent, traceState sql.NullString
	if err := scanner.Scan(&tenant, &run, &session, &turn, &message.DeliveryID, &message.Payload,
		&traceParent, &traceState, &message.AvailableAt, &message.CreatedAt); err != nil {
		return Message{}, err
	}
	var err error
	if message.TenantID, err = domain.ParseTenantID(tenant); err != nil {
		return Message{}, err
	}
	if message.RunID, err = domain.ParseRunID(run); err != nil {
		return Message{}, err
	}
	if message.SessionID, err = domain.ParseSessionID(session); err != nil {
		return Message{}, err
	}
	if message.TurnID, err = domain.ParseTurnID(turn); err != nil {
		return Message{}, err
	}
	message.Trace = TraceContext{TraceParent: traceParent.String, TraceState: traceState.String}
	message.AvailableAt, message.CreatedAt = message.AvailableAt.UTC(), message.CreatedAt.UTC()
	message.Payload = append(json.RawMessage(nil), message.Payload...)
	if err := validateMessage(message); err != nil {
		return Message{}, err
	}
	return message, nil
}

func scanDelivery(scanner rowScanner) (Delivery, error) {
	var delivery Delivery
	var tenant, session, turn, run string
	var traceParent, traceState sql.NullString
	if err := scanner.Scan(&tenant, &run, &session, &turn, &delivery.DeliveryID, &delivery.Payload,
		&traceParent, &traceState, &delivery.AvailableAt, &delivery.CreatedAt, &delivery.LeaseOwner,
		&delivery.LeaseToken, &delivery.LeaseExpires, &delivery.DeliveryCount); err != nil {
		return Delivery{}, err
	}
	var err error
	if delivery.TenantID, err = domain.ParseTenantID(tenant); err != nil {
		return Delivery{}, err
	}
	if delivery.RunID, err = domain.ParseRunID(run); err != nil {
		return Delivery{}, err
	}
	if delivery.SessionID, err = domain.ParseSessionID(session); err != nil {
		return Delivery{}, err
	}
	if delivery.TurnID, err = domain.ParseTurnID(turn); err != nil {
		return Delivery{}, err
	}
	delivery.Trace = TraceContext{TraceParent: traceParent.String, TraceState: traceState.String}
	delivery.AvailableAt, delivery.CreatedAt, delivery.LeaseExpires = delivery.AvailableAt.UTC(), delivery.CreatedAt.UTC(), delivery.LeaseExpires.UTC()
	delivery.Payload = append(json.RawMessage(nil), delivery.Payload...)
	if err := validateMessage(delivery.Message); err != nil || !validOwner(delivery.LeaseOwner) || delivery.LeaseToken == 0 || delivery.DeliveryCount == 0 {
		return Delivery{}, fmt.Errorf("%w: corrupt claimed delivery", ErrInvalidArgument)
	}
	return delivery, nil
}

func normalizeMessage(message Message) (Message, error) {
	if err := validateMessage(message); err != nil {
		return Message{}, err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(message.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return Message{}, err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return Message{}, err
	}
	message.Payload = payload
	message.CreatedAt = message.CreatedAt.UTC().Truncate(time.Microsecond)
	message.AvailableAt = message.AvailableAt.UTC().Truncate(time.Microsecond)
	return message, nil
}

func sameMessage(left, right Message) bool {
	return left.DeliveryID == right.DeliveryID && left.TenantID == right.TenantID && left.SessionID == right.SessionID &&
		left.TurnID == right.TurnID && left.RunID == right.RunID && bytes.Equal(left.Payload, right.Payload) && left.Trace == right.Trace &&
		left.AvailableAt.Equal(right.AvailableAt) && left.CreatedAt.Equal(right.CreatedAt)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

const messageColumns = `tenant_id,run_id,session_id,turn_id,delivery_id,payload,traceparent,tracestate,available_at,created_at`
const claimedDeliveryColumns = `message.tenant_id,message.run_id,message.session_id,message.turn_id,message.delivery_id,
    message.payload,message.traceparent,message.tracestate,message.available_at,message.created_at,
    message.lease_owner,message.lease_token,message.lease_expires_at,message.delivery_count`
const selectIdentitySQL = `SELECT ` + messageColumns + ` FROM run_queue_messages
WHERE tenant_id=$1 AND (run_id=$2 OR delivery_id=$3) LIMIT 1`
const pendingCountsSQL = `SELECT count(*) FILTER (WHERE tenant_id=$1),count(*) FROM run_queue_messages WHERE status IN ('READY','LEASED')`
const insertMessageSQL = `INSERT INTO run_queue_messages(tenant_id,run_id,session_id,turn_id,delivery_id,payload,
    traceparent,tracestate,available_at,created_at) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10)`
const claimSQL = `WITH candidates AS (
    SELECT message.tenant_id,message.run_id FROM run_queue_messages message
    WHERE message.tenant_id=$1 AND message.available_at<=$3
      AND (message.status='READY' OR (message.status='LEASED' AND message.lease_expires_at<=$3))
      AND NOT EXISTS (SELECT 1 FROM run_queue_messages earlier
          WHERE earlier.tenant_id=message.tenant_id AND earlier.session_id=message.session_id AND earlier.status<>'ACKED'
            AND (earlier.created_at,earlier.run_id)<(message.created_at,message.run_id))
    ORDER BY message.available_at,message.created_at,message.run_id
    FOR UPDATE SKIP LOCKED LIMIT $5
)
UPDATE run_queue_messages message SET status='LEASED',lease_owner=$2,lease_token=message.lease_token+1,
    lease_expires_at=$3+($4 * interval '1 millisecond'),delivery_count=message.delivery_count+1,last_error=NULL
FROM candidates WHERE message.tenant_id=candidates.tenant_id AND message.run_id=candidates.run_id
RETURNING ` + claimedDeliveryColumns
const renewSQL = `UPDATE run_queue_messages message SET lease_expires_at=$5+($6 * interval '1 millisecond')
WHERE message.tenant_id=$1 AND message.run_id=$2 AND message.status='LEASED' AND message.lease_owner=$3
    AND message.lease_token=$4 AND message.lease_expires_at>$5
RETURNING ` + claimedDeliveryColumns
const ackSQL = `UPDATE run_queue_messages SET status='ACKED',lease_owner=NULL,lease_expires_at=NULL,acknowledged_at=$5,last_error=NULL
WHERE tenant_id=$1 AND run_id=$2 AND status='LEASED' AND lease_owner=$3 AND lease_token=$4 AND lease_expires_at>$5`
const nackSQL = `UPDATE run_queue_messages SET status='READY',lease_owner=NULL,lease_expires_at=NULL,
    available_at=$5+($6 * interval '1 millisecond'),last_error=$7
WHERE tenant_id=$1 AND run_id=$2 AND status='LEASED' AND lease_owner=$3 AND lease_token=$4 AND lease_expires_at>$5`
const selectLeaseSQL = `SELECT status,lease_owner,lease_token,lease_expires_at FROM run_queue_messages WHERE tenant_id=$1 AND run_id=$2`
const cleanupSQL = `DELETE FROM run_queue_messages WHERE (tenant_id,run_id) IN (
    SELECT tenant_id,run_id FROM run_queue_messages WHERE tenant_id=$1 AND status='ACKED' AND acknowledged_at<$2
    ORDER BY acknowledged_at,run_id LIMIT $3)`

var _ RunQueue = (*PostgresAdapter)(nil)
