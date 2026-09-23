package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

type PushPayload struct {
	Type string `json:"type"`
	EventID string `json:"event_id"`
	MessageID string `json:"message_id"`
	ChannelID string `json:"channel_id"`
	AuthorID string `json:"author_id"`
	MessageContent string `json:"message_content"`
}

type PushTransport interface {
	Deliver(context.Context, models.PushRegistration, PushPayload) error
}

type papoRelayTransport struct{ client *http.Client }

func (t papoRelayTransport) Deliver(ctx context.Context, r models.PushRegistration, payload PushPayload) error {
	if r.Provider!=pushProviderPapoRelay || r.AuthToken==nil || validatePapoRelayAddress(r.Address)!=nil { return ErrPushDelivery }
	body,err:=json.Marshal(payload); if err!=nil { return err }
	req,err:=http.NewRequestWithContext(ctx,http.MethodPost,r.Address,bytes.NewReader(body)); if err!=nil { return err }
	req.Header.Set("Content-Type","application/json")
	req.Header.Set("Authorization","Bearer "+*r.AuthToken)
	resp,err:=t.client.Do(req); if err!=nil { return err }
	defer resp.Body.Close()
	_,_=io.CopyN(io.Discard,resp.Body,4096)
	if resp.StatusCode>=200 && resp.StatusCode<300 { return nil }
	if resp.StatusCode==http.StatusNotFound || resp.StatusCode==http.StatusGone {
		_ = storage.DeletePushRegistrationByID(ctx,r.ID)
	}
	return fmt.Errorf("%w: status=%d",ErrPushDelivery,resp.StatusCode)
}

var (
	pushTransportMu sync.RWMutex
	pushTransport PushTransport = papoRelayTransport{client:&http.Client{
		Timeout:5*time.Second,
		CheckRedirect:func(*http.Request,[]*http.Request) error{return http.ErrUseLastResponse},
	}}
)

func SetPushTransportForTests(t PushTransport) func() {
	pushTransportMu.Lock(); old:=pushTransport; pushTransport=t; pushTransportMu.Unlock()
	return func(){pushTransportMu.Lock();pushTransport=old;pushTransportMu.Unlock()}
}

func DeliverPushForNotifications(ctx context.Context, requestID string, deliveries []NotificationDelivery, message models.Message) {
	if len(deliveries)==0 { return }
	ids:=make([]string,0,len(deliveries)); seen:=map[string]bool{}
	for _,d:=range deliveries { if !seen[d.UserID] { seen[d.UserID]=true; ids=append(ids,d.UserID) } }
	regs,err:=storage.ListPushRegistrationsForUsers(ctx,ids)
	if err!=nil { utils.Errorf("request_id=%s push: falha ao listar registrations: %v",requestID,err); return }
	byUser:=map[string][]models.PushRegistration{}
	for _,r:=range regs { byUser[r.UserID]=append(byUser[r.UserID],r) }
	authorID:=""; if message.AuthorID!=nil { authorID=*message.AuthorID }
	for _,d:=range deliveries {
		p:=PushPayload{Type:"new_notification",EventID:d.EventID,MessageID:message.ID,ChannelID:message.ChannelID,AuthorID:authorID,MessageContent:d.MessageContent}
		for _,r:=range byUser[d.UserID] {
			pushTransportMu.RLock(); tr:=pushTransport; pushTransportMu.RUnlock()
			if err:=tr.Deliver(ctx,r,p); err!=nil && !errors.Is(err,context.Canceled) {
				utils.Errorf("request_id=%s push: entrega falhou provider=%s user_id=%s: %v",requestID,r.Provider,d.UserID,err)
			}
		}
	}
}
