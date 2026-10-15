package handlers

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ws "github.com/gorilla/websocket"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/storage"
	"papo/internal/websocket"
)

// readChannelViewsUpdate ignora presença e exige o payload existente com a lista completa de views.
func readChannelViewsUpdate(t *testing.T, conn *ws.Conn) websocket.ChannelUpdateOutbound {
	t.Helper()
	for {
		data := readWSMessage(t, conn)
		var event websocket.ChannelUpdateOutbound
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type != websocket.EventTypeChannelUpdate {
			continue
		}
		if event.Views == nil {
			t.Fatalf("views ausente: %s", data)
		}
		return event
	}
}

// assertNoChannelViewsUpdate garante que não leitores não recebem metadados nem URLs das views.
func assertNoChannelViewsUpdate(t *testing.T, conn *ws.Conn) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
				return
			}
			t.Fatal(err)
		}
		var event struct {
			Type websocket.EventType `json:"type"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == websocket.EventTypeChannelUpdate {
			t.Fatalf("update vazou para não leitor: %s", data)
		}
	}
}

// TestChannelViewsBroadcastTriggers cobre anexar, editar, destacar, revogar e retirar escopo.
// A revogação também deduplica canais, preserva views de outro app e notifica mais de um canal.
func TestChannelViewsBroadcastTriggers(t *testing.T) {
	go websocket.GetHub().Run()
	for _, trigger := range []string{"attach", "update", "detach", "scope_remove", "revoke"} {
		t.Run(trigger, func(t *testing.T) {
			cleanServers(context.Background())
			e := newApp()
			ownerID, owner := registerAndLogin(t, e)
			createServerFor(t, ownerID)
			readerID, reader := registerAndLogin(t, e)
			_, outsider := registerAndLogin(t, e)
			managerID, manager := registerAndLogin(t, e)
			manageRole := createRoleFor(t, models.RolePermissions{ManageApps: true})
			assignRoleToUser(t, managerID, manageRole.ID)
			role := createRoleFor(t, models.RolePermissions{})
			assignRoleToUser(t, readerID, role.ID)
			ch := createChannelFor(t, "ws_view_"+randHex(4))
			ch2 := createChannelFor(t, "ws_view2_"+randHex(4))
			for _, channel := range []models.Channel{ch, ch2} {
				if _, err := storage.UpdateChannelPermissions(context.Background(), channel.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
					t.Fatal(err)
				}
			}
			// O leitor comum só lê o primeiro canal, ao contrário do dono.
			blockedRole := createRoleFor(t, models.RolePermissions{})
			if _, err := storage.UpdateChannelPermissions(context.Background(), ch2.ID, role.ID, models.ChannelPermission{}); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.UpdateChannelPermissions(context.Background(), ch2.ID, blockedRole.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
				t.Fatal(err)
			}
			app, _, err := services.CreateApp(context.Background(), ownerID, "map", "view", "Mapa", models.AppCapabilities{AttachViews: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, channel := range []models.Channel{ch, ch2} {
				if err := services.AddAppScope(context.Background(), ownerID, app.ID, channel.ID); err != nil {
					t.Fatal(err)
				}
			}
			save := func(appID, channel string) models.ChannelAppView {
				t.Helper()
				v, err := services.SaveChannelView(context.Background(), ownerID, channel, "", appID, "https://8.8.8.8/map", testBaseURL, 0)
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			var initial models.ChannelAppView
			if trigger != "attach" {
				initial = save(app.ID, ch.ID)
			}
			var survivor models.ChannelAppView
			if trigger == "revoke" {
				save(app.ID, ch.ID)
				save(app.ID, ch2.ID)
				other, _, err := services.CreateApp(context.Background(), ownerID, "other", "view", "Outro", models.AppCapabilities{AttachViews: true})
				if err != nil {
					t.Fatal(err)
				}
				if err := services.AddAppScope(context.Background(), ownerID, other.ID, ch.ID); err != nil {
					t.Fatal(err)
				}
				survivor = save(other.ID, ch.ID)
			}
			srv := httptest.NewServer(e)
			defer srv.Close()
			dial := func(token string) *ws.Conn {
				t.Helper()
				header := http.Header{}
				header.Set("Cookie", "Auth="+token)
				conn, _, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", header)
				if err != nil {
					t.Fatal(err)
				}
				readWSMessage(t, conn)
				return conn
			}
			outsiderConn := dial(outsider)
			defer outsiderConn.Close()
			readerConn := dial(reader)
			defer readerConn.Close()
			ownerConn := dial(owner)
			defer ownerConn.Close()
			path := "/channels/" + ch.ID + "/views"
			body, _ := json.Marshal(map[string]any{"app_installation_id": app.ID, "url": "https://1.1.1.1/new", "position": 3})
			var rec *httptest.ResponseRecorder
			switch trigger {
			case "attach":
				rec = do(t, e, http.MethodPost, path, body, authCookie(owner))
			case "update":
				rec = do(t, e, http.MethodPut, path+"/"+initial.ID, body, authCookie(owner))
			case "detach":
				rec = do(t, e, http.MethodDelete, path+"/"+initial.ID, nil, authCookie(owner))
			case "scope_remove":
				rec = do(t, e, http.MethodDelete, "/apps/"+app.ID+"/channels/"+ch.ID, nil, authCookie(manager))
			case "revoke":
				rec = do(t, e, http.MethodDelete, "/apps/"+app.ID, nil, authCookie(manager))
			}
			if rec.Code != 200 && rec.Code != 201 && rec.Code != 204 {
				t.Fatalf("mutação: %d %s", rec.Code, rec.Body.String())
			}
			expected := 0
			if trigger == "attach" || trigger == "update" || trigger == "revoke" {
				expected = 1
			}
			check := func(event websocket.ChannelUpdateOutbound) {
				t.Helper()
				if event.ChannelID != ch.ID || event.Name != ch.Name || event.Position != ch.Position || event.Topic != nil {
					t.Fatalf("payload alterado: %+v", event)
				}
				if len(*event.Views) != expected {
					t.Fatalf("views esperadas %d: %+v", expected, *event.Views)
				}
				if trigger == "attach" || trigger == "update" {
					if (*event.Views)[0].URL != "https://1.1.1.1/new" || (*event.Views)[0].Position != 3 {
						t.Fatal(*event.Views)
					}
				}
				if trigger == "revoke" && (*event.Views)[0].ID != survivor.ID {
					t.Fatal("view de outro app perdida")
				}
			}
			check(readChannelViewsUpdate(t, readerConn))
			if trigger == "revoke" {
				seen := make(map[string]bool)
				for range 2 {
					event := readChannelViewsUpdate(t, ownerConn)
					if seen[event.ChannelID] {
						t.Fatal("canal notificado duas vezes")
					}
					seen[event.ChannelID] = true
					if event.ChannelID == ch.ID {
						check(event)
					} else if event.ChannelID != ch2.ID || len(*event.Views) != 0 {
						t.Fatalf("segundo canal: %+v", event)
					}
				}
				if !seen[ch.ID] || !seen[ch2.ID] {
					t.Fatal(seen)
				}
				assertNoChannelViewsUpdate(t, readerConn)
			} else {
				check(readChannelViewsUpdate(t, ownerConn))
			}
			assertNoChannelViewsUpdate(t, outsiderConn)
		})
	}
}
