package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"papo/internal/utils"
)

// Identidades de máquina (webhooks e apps remotos) compartilham o formato do
// token de acesso e o tratamento do avatar persistente.

// newMachineToken gera o token de acesso de uma identidade de máquina: 32
// bytes aleatórios em hex, devolvidos ao cliente uma única vez, e o SHA-256
// em hex, que é o único valor persistido.
func newMachineToken() (token, hash string, err error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(data)
	return token, machineTokenHash(token), nil
}

// machineTokenHash é o hash persistido de um token de identidade de máquina.
// A comparação com o valor armazenado deve ser em tempo constante.
func machineTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// machineAvatarArg converte o avatar em base64 de uma identidade de máquina no
// argumento de storage: avatar nil omite o campo (preserva o atual), vazio
// remove (NULL) e bytes válidos são gravados na media, devolvendo o sha_hash.
// O conteúdo precisa casar com avatarFormat, respeitar maxAvatarBytes e ser
// uma imagem válida; caso contrário retorna ErrInvalidInput. owner só compõe a
// mensagem de erro de gravação ("webhook", "app").
func machineAvatarArg(ctx context.Context, avatar *string, avatarFormat, owner string) (any, error) {
	if avatar == nil {
		return nil, nil
	}
	if *avatar == "" {
		return sql.NullString{Valid: false}, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(*avatar)
	if err != nil {
		return nil, ErrInvalidInput
	}
	upperFormat := normalizeImageFormat(avatarFormat)
	if !avatarContentMatchesFormat(decoded, upperFormat) || len(decoded) > maxAvatarBytes {
		return nil, ErrInvalidInput
	}
	if err := utils.ValidateImage(decoded, utils.MaxImageDimension); err != nil {
		return nil, ErrInvalidInput
	}
	sha, _, err := StoreMediaFromBytes(ctx, decoded, formatToMime(upperFormat))
	if err != nil {
		return nil, fmt.Errorf("falha ao gravar o avatar do %s: %w", owner, err)
	}
	return sql.NullString{String: sha, Valid: true}, nil
}
