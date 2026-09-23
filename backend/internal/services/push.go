package services

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"papo/internal/models"
	"papo/internal/storage"
)

const (
	pushProviderPapoRelay = "papo_relay"
	pushPlatformAndroid = "android"
	maxPushAddressLength = 4096
	maxPushTokenLength = 4096
)

type RegisterPushRequest struct {
	InstallationID string
	Provider string
	Address string
	AuthToken string
	Platform string
	AppVersion *string
}

type PushRegistrationInfo struct {
	ID string `json:"id"`
	InstallationID string `json:"installation_id"`
	Provider string `json:"provider"`
	Platform string `json:"platform"`
	AppVersion *string `json:"app_version,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func pushInfo(r models.PushRegistration) PushRegistrationInfo {
	return PushRegistrationInfo{ID:r.ID,InstallationID:r.InstallationID,Provider:r.Provider,Platform:r.Platform,AppVersion:r.AppVersion,CreatedAt:r.CreatedAt,UpdatedAt:r.UpdatedAt}
}

func validatePapoRelayAddress(raw string) error {
	if len(raw)==0 || len(raw)>maxPushAddressLength { return ErrInvalidInput }
	u, err := url.Parse(raw); if err != nil { return ErrInvalidInput }
	if u.Scheme!="https" || u.Hostname()!="push.papo.chat" || u.Port()!="" || u.User!=nil || u.RawQuery!="" || u.Fragment!="" {
		return ErrInvalidInput
	}
	if net.ParseIP(u.Hostname()) != nil || !strings.HasPrefix(u.EscapedPath(), "/v1/deliver/") || len(strings.TrimPrefix(u.EscapedPath(),"/v1/deliver/"))<8 {
		return ErrInvalidInput
	}
	return nil
}

func RegisterPush(ctx context.Context, actorID, targetID, connectionID string, req RegisterPushRequest) (PushRegistrationInfo, error) {
	if actorID=="" || actorID!=targetID || connectionID=="" { return PushRegistrationInfo{}, ErrPermissionDenied }
	if _,err:=uuid.Parse(req.InstallationID); err!=nil { return PushRegistrationInfo{}, ErrInvalidInput }
	if req.Provider!=pushProviderPapoRelay || req.Platform!=pushPlatformAndroid { return PushRegistrationInfo{}, ErrInvalidInput }
	if err:=validatePapoRelayAddress(req.Address); err!=nil { return PushRegistrationInfo{}, err }
	if len(req.AuthToken)<16 || len(req.AuthToken)>maxPushTokenLength { return PushRegistrationInfo{}, ErrInvalidInput }
	conn,err:=storage.GetUserConnectionByID(ctx,connectionID)
	if err!=nil || conn.UserID!=actorID || conn.ReplacedAt!=nil { return PushRegistrationInfo{}, ErrPermissionDenied }
	token:=req.AuthToken
	r,err:=storage.UpsertPushRegistration(ctx,actorID,connectionID,req.InstallationID,req.Provider,req.Address,&token,req.Platform,req.AppVersion)
	if err!=nil { return PushRegistrationInfo{}, err }
	return pushInfo(r),nil
}

func UnregisterPush(ctx context.Context, actorID,targetID,installationID string) error {
	if actorID=="" || actorID!=targetID { return ErrPermissionDenied }
	if _,err:=uuid.Parse(installationID); err!=nil { return ErrInvalidInput }
	return storage.DeletePushRegistration(ctx,targetID,installationID)
}

var ErrPushDelivery = errors.New("push delivery failed")
