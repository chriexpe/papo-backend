package worker

import (
	"papo/push/internal/models"
	"testing"
)

// TestAppInvitePush mantém a privacidade de preview e os seletores do convite.
func TestAppInvitePush(t *testing.T) {
	id := "convite"
	payload := models.PushJobPayload{Type: "app_invite", NotificationID: &id, AppInstallationID: "plugin", ChannelID: "canal", Author: "Jogo", Preview: "Vamos jogar?"}
	for _, tc := range []struct{ mode, body string }{{"full", "Jogo: Vamos jogar?"}, {"generic", "Jogo enviou um convite"}, {"hidden", "Novo convite"}} {
		t.Run(tc.mode, func(t *testing.T) {
			msg := buildMessage(payload, tc.mode)
			if msg.Body != tc.body || msg.Data["type"] != "app_invite" || msg.Data["notification_id"] != id || msg.Data["app_installation_id"] != "plugin" || msg.Data["channel_id"] != "canal" {
				t.Fatal(msg)
			}
		})
	}
}
