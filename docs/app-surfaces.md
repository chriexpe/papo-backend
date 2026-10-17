# Superfícies de app — plano v7

O canal **é** o app: não existe aba, toggle, timeline ou composer em canais `app`.
Uma superfície `embed` aparece como card dentro de uma mensagem de canal comum e
ativa somente ao clique. O frontend companheiro é a [PR #117](https://github.com/chriexpe/papo-frontend/pull/117).

## Integração com a PR 5

A PR 4 não descobre nem executa Wasm, não cria endpoint de enable/disable e não
interpreta `plugin.toml`. A PR 5 resolve a pasta validada e os metadados aprovados
através da interface `services.PluginSurfaceSource`:

```go
Resolve(context.Context, installationID string) (services.PluginSurfaceBundle, error)
```

O resultado contém `Directory`, `Digest`, `Enabled`, `ApprovedOrigins`, `Surfaces` e `UserActions`.
`SetPluginSurfaceSource` registra esse provedor no startup. Sem provedor, metadados
indisponíveis ou `Enabled=false`, a UI responde 404 e embeds históricos ficam inertes.
A migration 021 consolidou os tipos em `remote` e `plugin`. Somente `plugin` é aceito pelo provedor.

Os hooks são funções de pacote, sem publisher ou struct de serviço:

```go
CreateAppChannels(ctx, actorID, installationID, surfaces) ([]AppChannelChange, error)
ReconcileAppChannels(ctx, actorID, installationID, surfaces) ([]AppChannelChange, error)
DeleteAppChannels(ctx, actorID, installationID) ([]AppChannelChange, error)
```

Recebem a lista completa de declarações já resolvidas do manifest. `Kind` é
`channel` ou `embed`, `Source` é `bundled` ou `remote`; cada uma tem `ID` estável e
`Entry`. Canais têm `DefaultName`; superfícies embed exigem `Size` com `Width` e
`Height` positivos. O teto padrão do servidor é 560×420, configurável por
`APP_EMBED_MAX_WIDTH`/`APP_EMBED_MAX_HEIGHT`. `Load` aceita `click` (padrão) ou
`visible`; `Display` aceita `inline` (padrão) ou `overlay`. Overlay exige click.
`Poster` é um caminho opcional de imagem existente dentro de `ui/`.
`actorID` vazio registra operação de sistema.

As funções retornam somente após commit. `Before` guarda o canal/ACL anterior;
`After` guarda o payload novo. `Before=nil` significa criação; `After=nil`, exclusão;
ambos presentes, atualização. O chamador publica `channel_create`, `channel_update`
ou `channel_delete`. `handlers.BroadcastAppChannelChanges` é o adaptador usado nesta
PR. Um worker da PR 5 pode seguir o mesmo padrão: `ChannelReaders` para canais atuais,
`ChannelReadersSnapshot` para a ACL capturada antes de excluir e `hub.BroadcastToUsers`.
Nenhum broadcast fica na camada services/storage.

Reconciliação mantém ID, nome, posição e ACL por `surface_id`; diferenças de origem/
entrada atualizam o canal, remoções o apagam e novas superfícies criam canais no fim
da lista. Disable/remove chama o hook de exclusão; re-enable cria canais novos com
nomes padrão. App channels não exigem capacidade nem escopo. Remover escopo de envio
num canal comum não apaga canais app. Revoke captura e remove canais na própria
transação, remove escopos/backlog e retorna os snapshots para notificação.

Os endpoints normais continuam servindo para renomear, mover e editar ACL com
`manage_channels`. Exclusão manual e mudança de tipo são recusadas em português.
Envio/histórico de mensagens, voz e typing não são aceitos em `app`. `/channels`
inclui o objeto `app` somente para leitores; não existe mais campo `views` nem CRUD
`/channels/:id/views`.

## UI bundled e URLs remotas

`GET /apps/:installation_id/ui/:digest/*path` é **público** porque a WebView não tem sessão.
O digest é o da versão aprovada: as URLs chegam prontas nos payloads e mudam a cada
versão; um digest de outra versão responde 404.
Serve somente arquivos estáticos de `ui/`, nunca dados por usuário, código do plugin,
manifest ou templates. Não lê nem emite cookies. Traversal, diretórios, symlinks e
extensões fora do allowlist são recusados. Instalação desabilitada/revogada: 404.

Headers: `Content-Security-Policy: sandbox allow-scripts allow-forms; default-src 'self'`
mais a origem explícita configurada (`APPS_ORIGIN`, ou origem de `BaseURL` quando ausente)
e as origens remotas aprovadas; nunca `allow-same-origin`. A origem nunca vem de Host
ou Forwarded da requisição. Também envia `nosniff`,
`Referrer-Policy: no-referrer` e `Cross-Origin-Resource-Policy: cross-origin`. ETag combina
digest do plugin e caminho; `Cache-Control: public, max-age=31536000, immutable`, pois
a URL já identifica a versão. Respostas indisponíveis usam `no-store`.
Somente a UI estática envia `Access-Control-Allow-Origin: *`, sem
`Access-Control-Allow-Credentials`, inclusive com o CORS global ativo e na revalidação
304. Isso permite CSS, imagens, fontes, módulos JS e fetch de JSON a partir da origem
opaca do sandbox. O restante da API continua rejeitando CORS para `Origin: null`,
inclusive se a configuração incluir `*` ou `null`.

Superfícies remotas exigem HTTPS, máximo 2.048 caracteres, host diferente de `BASE_URL`,
sem localhost/redes privadas/reservadas, userinfo ou portas não padrão. O origin exato
precisa estar em `ApprovedOrigins`. O backend nunca baixa nem faz proxy da superfície
remota. Para BlueMap, a PR 5 fornece o destino configurado pelo administrador como
uma declaração `channel` de origem `remote`.

## Frames e cards nativos

`CreatePluginMessage` recebe a instalação do contexto interno do host. A instalação
precisa de `send_messages`, `post_app_embeds` e escopo num canal `text`/`voice` para
postar um frame (`source_type: app`). Ele referencia seu próprio `app_surface_id`,
declarado como `embed`; no máximo um por mensagem. Humanos, webhooks, superfícies
não declaradas e origens não aprovadas são recusados.

O autor declara o frame no manifest fornecido pela PR 5:

```toml
[[surfaces]]
kind = "embed"
id = "resultado"
entry = "ui/result.html"
size = { width = 480, height = 320 }
load = "click"
display = "overlay"
poster = "ui/result-poster.webp"
```

`size` é obrigatório e limitado ao máximo configurado do servidor (padrão 560×420).
Em inline, o cliente reduz proporcionalmente à largura da timeline. Em overlay,
size só dimensiona o card da timeline; o popup cobre a área do chat com as margens
das pills, sem esse limite autoral. Overlay é sempre aberto ao clique. O cliente
desenha a barra fina com identidade do autor, título opcional e fechar; Esc, fechar
e voltar no mobile encerram; trocar de canal destrói a superfície.

`load=click` mostra o poster ou card neutro com título até o clique, sem ícone de
play de vídeo. `load=visible` inicia ao entrar na área visível, somente em inline.
Uma única superfície fica ativa entre apps e rich-link players: a mais recentemente
ativada/visível vence; as demais mostram seus posters. Inline usa o clipping existente
e é destruído ao sair da tela/trocar de canal. O isolamento/CORS da UI não muda.

Poster é validado como PNG/JPEG/GIF/WebP/SVG existente em `ui/`, sem traversal ou
symlinks; no payload vira URL da rota pública de UI. O input da mensagem aceita
somente `source_type=app`, `app_surface_id` e `title` opcional (256 caracteres Unicode).
Tamanho, load, display e poster vêm da declaração, nunca de overrides na mensagem.

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

Não há header extra de provedor: o frame não carrega nome, avatar, provider, author,
thumbnail nem objeto `app` de canal. A mensagem continua tendo sua autoria normal.
O envelope mantém somente `id`/`source_type`; os demais campos são os do exemplo,
com `title` e `poster` opcionais. Entry remota continua sujeita a HTTPS/origens aprovadas.

O histórico guarda referência, size/load/display e caminho relativo do poster,
nunca a entry executável. Na leitura, entry é resolvida contra metadados atuais.
Disable/remove omite entry/poster, mantendo a referência, políticas e título como
card inerte. Um frame antigo que exceda um teto posteriormente reduzido também não
é lançado. A migration 021 usa `app_width`/`app_height` inteiros positivos, `app_load`,
`app_display` e `app_poster`; o CHECK também rejeita overlay+visible. O máximo
configurável é aplicado pelo serviço, não fixado no SQL.

**Cards customizados nativos continuam disponíveis para apps sem mudança**: título,
descrição, imagem, cor, fields e demais dados seguem o fluxo existente com
`source_type=custom`, sem exigir `post_app_embeds`. Para texto/imagem sem conteúdo web,
use esses cards; os campos nativos não são aceitos no frame.

O OpenAPI documenta o contrato completo de isolamento, links com confirmação e
fallback por plataforma. Não são implementados frontend, runtime, botões ou
`app_invalidate` nesta correção. Persistência futura da página pertence ao
`app_storage` da PR 5, sem bridge/SDK privilegiado.

## Interações e ponte HTTP confinada (§10)

A UI não recebe credenciais Papo ou bridge nativa. O cliente autenticado obtém um
 ticket de lançamento e o entrega somente no fragmento `#papo_ticket=...`.
As únicas rotas adicionais com CORS `*`, sem credenciais, são `session` e `rpc`;
nenhuma delas aceita cookies como autenticação. As demais rotas da API continuam
recusando origem opaca. O contrato e a revogação estão em
[app-interactions.md](app-interactions.md).
