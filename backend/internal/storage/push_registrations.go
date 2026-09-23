package storage

import (
	"context"
	"database/sql"
	"fmt"

	"papo/internal/models"
)

const pushRegistrationColumns = "id, user_id, connection_id, installation_id, provider, address, auth_token, platform, app_version, created_at, updated_at"

func scanPushRegistration(row rowScanner) (models.PushRegistration, error) {
	var r models.PushRegistration
	err := row.Scan(&r.ID, &r.UserID, &r.ConnectionID, &r.InstallationID, &r.Provider,
		&r.Address, &r.AuthToken, &r.Platform, &r.AppVersion, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func UpsertPushRegistration(ctx context.Context, userID, connectionID, installationID, provider, address string, authToken *string, platform string, appVersion *string) (models.PushRegistration, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil { return models.PushRegistration{}, err }
	defer tx.Rollback()

	// A relay capability can only be owned by one registration at a time.
	if _, err := tx.ExecContext(ctx, "DELETE FROM push_registrations WHERE provider=$1 AND address=$2 AND NOT (user_id=$3 AND installation_id=$4)", provider, address, userID, installationID); err != nil {
		return models.PushRegistration{}, fmt.Errorf("falha ao transferir push registration: %w", err)
	}
	r, err := scanPushRegistration(tx.QueryRowContext(ctx,
		`INSERT INTO push_registrations
		(user_id, connection_id, installation_id, provider, address, auth_token, platform, app_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (user_id, installation_id, provider) DO UPDATE SET
		  connection_id=EXCLUDED.connection_id, address=EXCLUDED.address,
		  auth_token=EXCLUDED.auth_token, platform=EXCLUDED.platform,
		  app_version=EXCLUDED.app_version, updated_at=NOW()
		RETURNING `+pushRegistrationColumns,
		userID, connectionID, installationID, provider, address, authToken, platform, appVersion))
	if err != nil { return models.PushRegistration{}, mapStorageError(err) }
	if err := tx.Commit(); err != nil { return models.PushRegistration{}, err }
	return r, nil
}

func DeletePushRegistration(ctx context.Context, userID, installationID string) error {
	_, err := GetDB().ExecContext(ctx, "DELETE FROM push_registrations WHERE user_id=$1 AND installation_id=$2", userID, installationID)
	return err
}

func DeletePushRegistrationByID(ctx context.Context, id string) error {
	_, err := GetDB().ExecContext(ctx, "DELETE FROM push_registrations WHERE id=$1", id)
	return err
}

func ListPushRegistrationsForUsers(ctx context.Context, userIDs []string) ([]models.PushRegistration, error) {
	if len(userIDs)==0 { return []models.PushRegistration{}, nil }
	rows, err := GetDB().QueryContext(ctx, "SELECT "+pushRegistrationColumns+" FROM push_registrations WHERE user_id = ANY($1)", userIDs)
	if err != nil { return nil, err }
	defer rows.Close()
	out := make([]models.PushRegistration,0)
	for rows.Next() {
		r, err := scanPushRegistration(rows); if err != nil { return nil, err }
		out=append(out,r)
	}
	return out, rows.Err()
}

func movePushRegistrationsTx(ctx context.Context, tx *sql.Tx, oldID, newID string) error {
	_, err := tx.ExecContext(ctx, "UPDATE push_registrations SET connection_id=$2, updated_at=NOW() WHERE connection_id=$1", oldID, newID)
	return err
}

func deletePushRegistrationsForConnectionTx(ctx context.Context, tx *sql.Tx, userID, connectionID string) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM push_registrations WHERE user_id=$1 AND connection_id=$2", userID, connectionID)
	return err
}

func deletePushRegistrationsForUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM push_registrations WHERE user_id=$1", userID)
	return err
}
