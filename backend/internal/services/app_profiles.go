package services

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"papo/internal/models"
	"papo/internal/storage"
)

// callerProfile não recebe uma identidade do guest e não expõe structs de usuário completas.
func callerProfile(ctx context.Context, id string) (models.AppUserProfile, error) {
	list, err := storage.AppPublicProfiles(ctx, []string{id})
	if err != nil {
		return models.AppUserProfile{}, err
	}
	if len(list) != 1 {
		return models.AppUserProfile{}, ErrPermissionDenied
	}
	return list[0], nil
}

// ResolveUsers omite ausentes e sem acesso da mesma forma, preservando a ordem sem duplicatas.
func (m *PluginManager) ResolveUsers(ctx context.Context, id string, ids []string) ([]models.AppUserProfile, error) {
	if len(ids) > 50 {
		return nil, ErrInvalidInput
	}
	unique := []string{}
	seen := map[string]bool{}
	for _, s := range ids {
		u, err := uuid.Parse(s)
		if err != nil {
			return nil, ErrInvalidInput
		}
		s = u.String()
		if !seen[s] {
			unique = append(unique, s)
			seen[s] = true
		}
	}
	a, err := activeApp(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Kind != "plugin" || a.PluginState != "enabled" {
		return nil, ErrPermissionDenied
	}
	if err = m.limit("users.resolve:"+id, m.Config.Limits.UsersResolvePerPlugin); err != nil {
		return nil, err
	}
	channels, err := storage.AppProfileChannels(ctx, id)
	if err != nil {
		return nil, err
	}
	profiles, err := storage.AppPublicProfiles(ctx, unique)
	if err != nil {
		return nil, err
	}
	allowed := map[string]models.AppUserProfile{}
	for _, channel := range channels {
		pending := []string{}
		for _, p := range profiles {
			if _, ok := allowed[p.ID]; !ok {
				pending = append(pending, p.ID)
			}
		}
		if len(pending) == 0 {
			break
		}
		readers, err := ChannelReaders(ctx, channel, pending)
		if errors.Is(err, ErrChannelNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, p := range profiles {
			if readers[p.ID] {
				allowed[p.ID] = p
			}
		}
	}
	out := []models.AppUserProfile{}
	for _, s := range unique {
		if p, ok := allowed[s]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}
