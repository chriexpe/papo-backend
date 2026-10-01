package websocket

import (
	"sort"
	"sync"
	"time"
)

// ActivityStore mantém Rich Presence efêmero por conexão. O mesmo usuário
// pode publicar em vários aparelhos; a publicação mais recente vence e,
// quando ela some, a publicação anterior ainda conectada volta a ser efetiva.
type ActivityStore struct {
	mu     sync.RWMutex
	seq    uint64
	byUser map[string]map[string]activityRecord
}

type activityRecord struct {
	seq      uint64
	activity ActivityPayload
}

func NewActivityStore() *ActivityStore {
	return &ActivityStore{byUser: make(map[string]map[string]activityRecord)}
}

func (s *ActivityStore) Set(userID, clientID string, activity *ActivityPayload) (*ActivityPayload, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	connections := s.byUser[userID]
	if activity == nil {
		if connections == nil {
			return nil, false
		}
		if _, ok := connections[clientID]; !ok {
			return cloneActivityPtr(s.effectiveLocked(connections)), false
		}
		delete(connections, clientID)
		if len(connections) == 0 {
			delete(s.byUser, userID)
			return nil, true
		}
		return cloneActivityPtr(s.effectiveLocked(connections)), true
	}

	if connections == nil {
		connections = make(map[string]activityRecord)
		s.byUser[userID] = connections
	}
	s.seq++
	connections[clientID] = activityRecord{seq: s.seq, activity: cloneActivity(*activity)}
	effective := cloneActivity(*activity)
	return &effective, true
}

func (s *ActivityStore) effectiveLocked(connections map[string]activityRecord) *ActivityPayload {
	var best *activityRecord
	var bestID string
	for clientID, record := range connections {
		if best == nil || record.seq > best.seq || (record.seq == best.seq && clientID > bestID) {
			copy := record
			best = &copy
			bestID = clientID
		}
	}
	if best == nil {
		return nil
	}
	activity := cloneActivity(best.activity)
	return &activity
}

func (s *ActivityStore) Snapshot() []ActivityMember {
	s.mu.RLock()
	defer s.mu.RUnlock()
	members := make([]ActivityMember, 0, len(s.byUser))
	for userID, connections := range s.byUser {
		if activity := s.effectiveLocked(connections); activity != nil {
			members = append(members, ActivityMember{UserID: userID, Activity: *activity})
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].UserID < members[j].UserID })
	return members
}

func cloneActivityPtr(activity *ActivityPayload) *ActivityPayload {
	if activity == nil {
		return nil
	}
	cloned := cloneActivity(*activity)
	return &cloned
}

func cloneActivity(activity ActivityPayload) ActivityPayload {
	activity.Details = cloneActivityString(activity.Details)
	activity.State = cloneActivityString(activity.State)
	activity.Image = cloneActivityString(activity.Image)
	activity.StartedAt = cloneActivityTime(activity.StartedAt)
	activity.EndsAt = cloneActivityTime(activity.EndsAt)
	return activity
}

func cloneActivityString(value *string) *string {
	if value == nil { return nil }
	copy := *value
	return &copy
}

func cloneActivityTime(value *time.Time) *time.Time {
	if value == nil { return nil }
	copy := *value
	return &copy
}
