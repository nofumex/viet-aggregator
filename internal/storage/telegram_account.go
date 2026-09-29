package storage

import (
	"context"
	"github.com/nofumex/telegram-aggregator/internal/domain"
)

func (s *Store) TelegramAccount(ctx context.Context) (domain.TelegramAccount, error) {
	var a domain.TelegramAccount
	err := s.DB.QueryRow(ctx, `SELECT api_id,api_hash,phone,status,last_error,connected_at,updated_at FROM telegram_account WHERE id=1`).Scan(&a.APIID, &a.APIHash, &a.Phone, &a.Status, &a.LastError, &a.ConnectedAt, &a.UpdatedAt)
	return a, err
}
func (s *Store) SaveTelegramAccount(ctx context.Context, apiID int, apiHash, phone string) error {
	_, err := s.DB.Exec(ctx, `UPDATE telegram_account SET api_id=$1,api_hash=$2,phone=$3,status='disconnected',last_error='',updated_at=now() WHERE id=1`, apiID, apiHash, phone)
	return err
}
func (s *Store) SetTelegramAccountStatus(ctx context.Context, status, lastError string) error {
	_, err := s.DB.Exec(ctx, `UPDATE telegram_account SET status=$1,last_error=$2,connected_at=CASE WHEN $1='authorized' THEN now() ELSE connected_at END,updated_at=now() WHERE id=1`, status, lastError)
	return err
}
func (s *Store) LoadMTProtoSession(ctx context.Context) ([]byte, error) {
	var data []byte
	err := s.DB.QueryRow(ctx, `SELECT session_data FROM telegram_account WHERE id=1`).Scan(&data)
	return data, err
}
func (s *Store) SaveMTProtoSession(ctx context.Context, data []byte) error {
	_, err := s.DB.Exec(ctx, `UPDATE telegram_account SET session_data=$1,updated_at=now() WHERE id=1`, data)
	return err
}
func (s *Store) ClearMTProtoSession(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `UPDATE telegram_account SET session_data=NULL,status='disconnected',last_error='',connected_at=NULL,updated_at=now() WHERE id=1`)
	return err
}
