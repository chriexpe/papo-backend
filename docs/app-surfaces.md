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

O resultado contém `Directory`, `Digest`, `Enabled`, `ApprovedOrigins` e `Surfaces`.
`SetPluginSurfaceSource` registra esse provedor no startup. Sem provedor, metadados
indisponíveis ou `Enabled=false`, a UI responde 404 e embeds históricos ficam inertes.
Nesta etapa, os testes fornecem pastas fixture com `plugin.wasm` e `ui/`. O tipo
legado `wasm` continua sendo o registro sem token até a migration de plugins da PR 5
introduzir `plugin`; `remote`/`view` não são aceitos como plugins pelo provedor.

Os hooks são funções de pacote, sem publisher ou struct de serviço:

```go
CreateAppChannels(ctx, actorID, installationID, surfaces) ([]AppChannelChange, error)
ReconcileAppChannels(ctx, actorID, installationID, surfaces) ([]AppChannelChange, error)
DeleteAppChannels(ctx, actorID, installationID) ([]AppChannelChange, error)
```

Recebem a lista completa de declarações já resolvidas do manifest. `Kind` é
`channel` ou `embed`, `Source` é `bundled` ou `remote`; cada uma tem `ID` estável e
`Entry`. Canais têm `DefaultName`; embeds têm `FrameWidth`/`FrameHeight` (padrões
480×320, limites 1920×1080). `actorID` vazio registra operação de sistema.

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

`GET /apps/:installation_id/ui/*path` é **público** porque a WebView não tem sessão.
Serve somente arquivos estáticos de `ui/`, nunca dados por usuário, código do plugin,
manifest ou templates. Não lê nem emite cookies. Traversal, diretórios, symlinks e
extensões fora do allowlist são recusados. Instalação desabilitada/revogada: 404.

Headers: `Content-Security-Policy: sandbox allow-scripts allow-forms; default-src 'self' <origem-de-BaseURL>`
mais somente origens aprovadas; nunca `allow-same-origin`. Também envia `nosniff`,
`Referrer-Policy: no-referrer` e `Cross-Origin-Resource-Policy: cross-origin`.
Somente esta rota envia `Access-Control-Allow-Origin: *`, sem credenciais, para assets
de documentos sandbox com origem opaca. A origem explícita vem de `BaseURL`, nunca
de Host/Origin da requisição. ETag combina
digest do plugin e caminho; `Cache-Control: public, max-age=0, must-revalidate` revalida
antes de reutilizar conteúdo. Respostas indisponíveis usam `no-store`.
A API rejeita CORS para `Origin: null`, inclusive se a configuração incluir `*` ou `null`.

Superfícies remotas exigem HTTPS, máximo 2.048 caracteres, host diferente de `BASE_URL`,
sem localhost/redes privadas/reservadas, userinfo ou portas não padrão. O origin exato
precisa estar em `ApprovedOrigins`. O backend nunca baixa nem faz proxy da superfície
remota. Para BlueMap, a PR 5 fornece o destino configurado pelo administrador como
uma declaração `channel` de origem `remote`.

## Embeds inline

`CreatePluginMessage` recebe a instalação do contexto interno do host, nunca do corpo
de uma requisição humana. A instalação precisa de `send_messages`, `post_app_embeds`
e escopo num canal `text`/`voice`. Um input `source_type: app` referencia somente seu
próprio `app_surface_id` declarado como `embed`; no máximo um por mensagem. Humanos,
webhooks, superfícies não declaradas e origens não aprovadas são recusados.

O histórico guarda a referência, nunca a entrada executável. Na leitura, a entrada é
resolvida contra os metadados atuais. Disable/remove mantém o card com `available=false`
e sem `entry`, `url` ou `embed_url` executável. Miniatura opcional usa o fluxo de mídia
SSRF-safe existente. Persistência futura pertence ao `app_storage` da PR 5, sem bridge
ou SDK privilegiado na página.

O OpenAPI documenta o contrato completo de isolamento, clipping, uma superfície ativa
por vez, links externos com confirmação e fallback por plataforma. Não são implementados
frontend, runtime de plugins ou descoberta nesta PR.

## Migration do fork

A 019 foi corrigida em lugar de acrescentar uma segunda migration sobre o modelo
rejeitado. Bancos descartáveis devem ser recriados. Quem já aplicou a 019 antiga deve
fazer rollback usando a revisão antiga antes de trocar para esta versão. Não executar
um Down novo contra o schema da antiga `channel_app_views`.
