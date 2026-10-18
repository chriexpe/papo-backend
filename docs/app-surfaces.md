# Superfícies de app

Um app expõe superfícies de duas formas:

- **canal app** (`kind = "channel"`): o canal *é* o app. Não tem conversa, composer,
  voz nem typing; o cliente abre a superfície no lugar da timeline.
- **frame em mensagem** (`kind = "embed"`): um embed `source_type: app` dentro de uma
  mensagem de canal comum, aberto por clique (ou ao ficar visível, em inline).

Somente instalações `plugin` têm superfícies. Os metadados vêm do provedor
`services.PluginSurfaceSource`, registrado no startup por `SetPluginSurfaceSource`:

```go
Resolve(context.Context, installationID string) (services.PluginSurfaceBundle, error)
```

O resultado contém `Directory`, `Digest`, `Enabled`, `ApprovedOrigins` e `Surfaces`.
Sem provedor, com metadados indisponíveis ou `Enabled=false`, a UI responde 404 e os
frames históricos ficam inertes.

## Canais app

O ciclo de vida dos canais é feito por funções de pacote, chamadas por quem
habilita, atualiza ou desabilita a instalação:

```go
CreateAppChannels(ctx, actorID, installationID, surfaces) ([]AppChannelChange, error)
ReconcileAppChannels(ctx, actorID, installationID, surfaces) ([]AppChannelChange, error)
DeleteAppChannels(ctx, actorID, installationID) ([]AppChannelChange, error)
```

Recebem a lista completa de declarações já resolvidas. `Kind` é `channel` ou `embed`,
`Source` é `bundled` ou `remote`; cada uma tem `ID` estável e `Entry`. Canais têm
`DefaultName`. `actorID` vazio registra uma operação de sistema.

As funções retornam somente após o commit. Em cada `AppChannelChange`, `Before` guarda
o canal e a ACL anteriores e `After` o payload novo: `Before=nil` é criação,
`After=nil` é exclusão e os dois presentes, atualização. O chamador publica
`channel_create`, `channel_update` ou `channel_delete` somente aos leitores
(`ChannelReaders` para canais atuais, `ChannelReadersSnapshot` para a ACL capturada
antes de excluir); services e storage não fazem broadcast.
`handlers.BroadcastAppChannelChanges` é esse adaptador.

A reconciliação usa `surface_id`: preserva id, nome, posição e ACL; mudanças de origem
ou entrada atualizam o canal; superfícies removidas apagam o canal e novas criam canais
no fim da lista. Desabilitar remove os canais; reabilitar cria canais novos com os nomes
padrão. Canais app não dependem de capacidades nem de escopo. Revogar a instalação
captura e remove os canais na mesma transação e devolve os snapshots para notificação.

Renomear, mover e editar ACL usam os endpoints normais (`manage_channels`). Excluir
manualmente ou mudar o tipo de um canal app é recusado. `GET /channels` inclui o objeto
`app` somente para quem lê o canal.

## UI bundled

`GET /apps/:installation_id/ui/*path` é **público**: a página roda num iframe sandbox
sem sessão. Serve somente arquivos estáticos de `ui/`, nunca dados por usuário, código
do plugin, manifest ou templates, e não lê nem emite cookies. Traversal, diretórios,
symlinks e extensões fora do allowlist são recusados; instalação desabilitada ou
revogada responde 404.

Headers:

- `Content-Security-Policy: sandbox allow-scripts allow-forms; default-src 'self'`
  mais a origem configurada (`APPS_ORIGIN`, ou a de `BASE_URL`) e as origens remotas
  aprovadas. Nunca `allow-same-origin`; a origem nunca vem de `Host`/`Forwarded`.
- `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`,
  `Cross-Origin-Resource-Policy: cross-origin`.
- `Access-Control-Allow-Origin: *` sem `Access-Control-Allow-Credentials`, inclusive
  em 304, para CSS, imagens, fontes, módulos e fetch de JSON a partir da origem opaca.
  O restante da API continua recusando `Origin: null`.
- `ETag` derivado do digest do plugin e do caminho, com revalidação; respostas
  indisponíveis usam `no-store`.

## Superfícies remotas

Exigem HTTPS, no máximo 2.048 caracteres, host diferente de `BASE_URL`, sem
localhost/redes privadas/reservadas, userinfo ou porta não padrão, e a origem exata em
`ApprovedOrigins`. O backend nunca baixa nem faz proxy da superfície remota.

## Frames em mensagens

Só o próprio plugin posta um frame (`CreatePluginMessage`, com a instalação vinda do
contexto interno do host): precisa de `send_messages`, `post_app_embeds` e escopo no
canal `text`/`voice`; no máximo um frame por mensagem, referenciando uma superfície
`embed` declarada por ele. Humanos, webhooks, superfícies não declaradas e origens não
aprovadas são recusados.

Declaração no manifest:

```toml
[[surfaces]]
kind = "embed"
id = "resultado"
entry = "ui/result.html"
size = { width = 480, height = 320 }
load = "click"      # click (padrão) ou visible
display = "overlay" # inline (padrão) ou overlay
poster = "ui/result-poster.webp"
```

- `size` é obrigatório e limitado pelo teto do servidor (padrão 560×420,
  `APP_EMBED_MAX_WIDTH`/`APP_EMBED_MAX_HEIGHT`). É o tamanho do card na mensagem.
- `overlay` exige `load = "click"`; `visible` só existe em inline.
- `poster` é opcional: imagem PNG/JPEG/GIF/WebP/SVG existente em `ui/`.

A mensagem informa somente `source_type: "app"`, `app_surface_id` e `title` opcional
(até 256 caracteres); tamanho, carregamento, exibição e poster vêm sempre da
declaração. Payload na leitura:

```json
{
  "id": "<embed_id>",
  "source_type": "app",
  "surface": {"app_installation_id": "<installation_id>", "id": "resultado"},
  "entry": "/apps/<installation_id>/ui/result.html",
  "size": {"width": 480, "height": 320},
  "load": "click",
  "display": "overlay",
  "poster": "/apps/<installation_id>/ui/result-poster.webp",
  "title": "Resultado da rodada"
}
```

O frame não carrega nome, avatar, provedor nem autor: a autoria é a da mensagem.
O histórico guarda a referência, as políticas e o caminho do poster, nunca a entry
executável; a entry é resolvida na leitura contra os metadados atuais. Com a instalação
desabilitada ou removida, `entry` e `poster` são omitidos e o card fica inerte; um frame
antigo acima de um teto reduzido depois também não é lançado.

Cards customizados nativos (`source_type: custom`: título, descrição, imagem, cor,
campos) continuam disponíveis aos apps sem `post_app_embeds`; são a opção para conteúdo
sem web. Os campos nativos não são aceitos no frame.
