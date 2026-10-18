# Plugins de pasta

Plugins (`kind = plugin`) são pastas em `PLUGINS_DIR` com `plugin.wasm` e
`plugin.toml`, executadas pelo próprio backend depois de um consentimento
explícito; apps `remote` continuam separados, com token próprio. O schema está
em `migrations/022_app_plugins.sql`.

## Pasta e ferramentas

```text
plugins/<id>/
  plugin.wasm       obrigatório; ausência significa que a pasta não é plugin
  plugin.toml       manifest v1 obrigatório
  icon.png          opcional; ou icon.webp, copiado para media no enable
  ui/               superfícies públicas estáticas
  templates/        assets privados de runtime
  locales/          assets privados de runtime
```

A pasta é somente leitura para o guest; estado fica em `app_storage`.
`PLUGINS_DIR` é uma pasta/volume confiável do operador, não é um diretório para
uploads arbitrários de usuários. Descoberta acontece no startup e no rescan;
`PLUGINS_HOT_RELOAD=true` adiciona polling a cada segundo (padrão false).
Nenhum módulo é instanciado para validar/descobrir. Remoção de um plugin já ativo
é uma transição de lifecycle e tenta on_disable somente se ainda houver os bytes aprovados.

```bash
TINYGO=/caminho/tinygo/bin/tinygo examples/plugins/build.sh
cd backend
go run ./cmd/papo-plugin check ../examples/plugins/reference
go run ./cmd/papo-plugin check ../examples/plugins/bluemap
go run ./cmd/papo-plugin pack ../examples/plugins/reference /tmp/reference.papoplugin
```

Check, pack e discovery compartilham exatamente o validador. Pacotes contêm uma
única pasta `<id>/`, e o nome é `<id>.papoplugin`. Extração usa staging, limites
antes/durante a leitura, validação completa e rename. Não sobrescreve uma pasta
existente: depois de extraído, a pasta é canônica; para upgrade por pacote, o operador
substitui explicitamente a pasta anterior, com o plugin desabilitado.
Pack valida o ZIP produzido novamente e compara seu digest antes de publicar.
Arquivos locais `.go`/`go.mod` podem acompanhar os exemplos; nunca são executados.

## Manifest v1

```toml
manifest_version = 1
id = "reference"
name = "Plugin de referência"
version = "1.0.0"
min_papo_api = 1
# remote_origins = ["https://maps.example.org"] # campo da raiz, antes das tabelas

[permissions]
request = ["send_messages", "post_app_embeds", "private_storage"]

[runtime]
max_memory_mb = 16

[[settings]]
key = "target_channel"
type = "channel"
required = true

[[surfaces]]
id = "home"
kind = "channel"
default_name = "plugin-reference"
entry = "ui/index.html"

[[surfaces]]
id = "result"
kind = "embed"
entry = "ui/result.html"
size = { width = 480, height = 320 }
load = "click"
display = "overlay"
```

IDs usam `[a-z0-9][a-z0-9._-]{0,99}` e coincidem com a pasta. Versão SemVer;
API mínima suportada: 1. Campos desconhecidos e IDs duplicados são recusados.
Settings aceitam `string`, `boolean`, `integer`, `https_url`, `channel`, com
`required` e `default`. Um setting `channel` vira **escopo visível no preview** e
só é concedido pelo enable aprovado. Não concede capacidade send/read sozinho.

Cada superfície escolhe exatamente uma fonte: `entry` dentro de `ui/`,
`entry_url` HTTPS de origem declarada, ou `entry_url_setting` referindo um setting
`https_url`. Origens desses settings entram explicitamente no consentimento.
As regras de URL/DNS/host são as das superfícies remotas (docs/app-surfaces.md); não há proxy remoto. Canal exige
`default_name`; embed exige size positivo e segue os tetos do servidor (padrão 560×420), load e display.

## Configuração

Limites em bytes, todos lidos no startup; valores inválidos impedem inicialização:

| Env | Padrão |
|---|---:|
| `PLUGINS_DIR` | `./plugins` |
| `PLUGINS_HOT_RELOAD` | `false` |
| `PLUGINS_MAX_WASM_BYTES` | 16777216 (16 MiB) |
| `PLUGINS_MAX_FOLDER_BYTES` | 67108864 (64 MiB) |
| `PLUGINS_MAX_FILES` | 512 |
| `PLUGINS_MAX_MANIFEST_BYTES` | 65536 |
| `PLUGINS_MAX_ASSET_BYTES` | 4194304 |
| `PLUGINS_MAX_ICON_BYTES` | 2097152 |
| `PLUGINS_ASSET_READ_BYTES` | 32768, pode reduzir; nunca ultrapassa 32 KiB |
| `PLUGINS_DEFAULT_MEMORY_MB` | 16 |
| `PLUGINS_MAX_MEMORY_MB` | 64 (teto absoluto do adaptador: 256) |
| `PLUGINS_ALLOWED_PERMISSIONS` | send_messages,read_messages,post_app_embeds,private_storage,send_invites |
| `PLUGINS_BLOCKED_PERMISSIONS` | vazio |

Allowlist ausente usa os quatro nomes conhecidos; allowlist explicitamente vazia
nega tudo. Blocked sempre prevalece. O consentimento pode restringir ainda mais.
O override de memória não excede o pedido no manifest nem o teto global.

Rejeitam-se links, arquivos especiais, traversal, caminhos não canônicos/absolutos,
colisões de nomes ignorando caixa e extensões de UI fora do allowlist da rota de UI.
Entradas de diretório também são limitadas (até `2*PLUGINS_MAX_FILES+1`). Ícones
precisam decodificar como PNG/WebP, dimensão até 4096². O digest SHA-256 usa arquivos
ordenados, comprimento/caminho, comprimento/conteúdo; inclui manifest, ícone, UI,
templates, locales e todos os demais arquivos.

Ao habilitar (ou no hot reload), a pasta é copiada para
`PLUGINS_DIR/.approved/<digest>/<id>` e a cópia é validada e hasheada de novo;
ela só é publicada se o digest for o do consentimento. Execução, `asset.read`,
UI e superfícies usam somente essa cópia, carregada uma vez em memória. Editar a
pasta viva não afeta nada até o próximo rescan (manual, no startup ou pelo watch
do hot reload). Cópias que não pertencem a um plugin habilitado são removidas
no rescan. Pastas que começam com `.` não são candidatas à descoberta.

## Endpoints e consentimento

Todas as rotas `/plugins` exigem JWT + `manage_apps`:

| Método | Rota | Efeito |
|---|---|---|
| GET | `/plugins` | Catálogo, erros e instalação/estado |
| POST | `/plugins/rescan` | Descoberta e reconciliação |
| POST | `/plugins/:id/preview` | Consentimento, sem efeitos de guest |
| POST | `/plugins/:id/enable` | Aprova e habilita/reaprova |
| PATCH | `/plugins/:id` | Settings/memória de plugin habilitado |
| POST | `/plugins/:id/disable` | Callback limitado e remoção de grants/canais |
| POST | `/plugins/:id/trigger` | Invoca on_trigger com o JSON de entrada |

Preview recebe `settings`, `permissions` opcional (subconjunto do pedido),
`scopes` explícitos opcionais e `max_memory_mb`. Retorna todos os grants efetivos,
permissões bloqueadas, escopos explícitos + derivados dos settings, origens,
superfícies/canais, digest, estado, revisão, `consent` e `expires_at`.

Enable recebe a **mesma seleção** e os campos `consent`/`expires_at` do preview.
O HMAC vincula ator e payload completo; expira em 15 minutos e invalida em restart.
Mudança de arquivo, política, revisão, settings, escopo ou origem rejeita aprovação
antiga (409). A leitura humana de cada canal é verificada novamente no enable.
Grants/settings/escopos são persistidos em uma transação; depois os hooks de canais app
reconciliam canais e executa-se on_enable. Falha de callback/hook remove grants e
canais em cleanup com contexto próprio, mesmo que a requisição tenha sido cancelada.
Efeitos de mensagem já confirmados não são revertidos.

PATCH aceita apenas settings/memória; preserva os grants atuais. Mudança de escopos
ou origens exige gerar preview para a seleção nova e reenviar consent/expires_at.
Para grants previamente reduzidos, use esse mesmo subconjunto em `permissions` no
preview; PATCH preserva-o automaticamente. Trocar só URL no mesmo origin não amplia
autoridade e não requer consentimento novo. Renomear/reordenar canais app continua
nas rotas normais de gestão de canais; reconciliação conserva ID/nome/posição/ACL
quando `surface_id` persiste.

No rescan, sem hot reload, um digest novo em plugin habilitado põe em quarentena
até reaprovação. Com hot reload (watch de 1 s), sem expansão do pedido de
permissões/origens/escopos, a versão nova é aprovada e aplicada automaticamente;
expansão exige consentimento. on_disable sempre executa a versão aprovada. Quarentena
persiste no banco, conserva canais (indisponíveis) e impede novas invocações.
Disable/remove revoga grants, apaga backlog/escopos/canais e deixa embeds históricos
inertes. Falha de on_disable é reportada, mas não interrompe esse cleanup.

As rotas antigas de upload e trigger `/apps/:id/artifact` e `/apps/:id/trigger`
foram removidas. POST `/apps` instala somente remote; mutações de grants/escopos
legadas rejeitam plugins para não contornar consentimento.

## Runtime, ABI e host API

`PluginHost` continua independente do wazero nos serviços. Wazero v1.12.0,
TinyGo 0.43.0, core Wasm; contrato em `papo-plugin.wit`, sem Component Model.

```text
_initialize() -> ()
alloc(len:i32) -> ptr:i32
free(ptr:i32) -> ()
invoke(ptr:i32,len:i32) -> packed:i64  # ptr_out=packed>>32; len_out=packed&0xffffffff
papo_host_v1.call(req_ptr:i32,req_len:i32,out_ptr:i32,out_cap:i32) -> len:i32
```

Instância nova a cada callback, com `_initialize` uma vez. Host copia e libera os
buffers de entrada/saída; falha/timeout fecha a instância. Não tenta free depois de
fechamento. Scratch da importação pertence ao guest. Se capacity for insuficiente,
retorna tamanho necessário sem escrever; **efeitos já podem ter ocorrido**: nunca
repetir automaticamente uma escrita. Entrada/saída JSON UTF-8, resposta com `ok` booleano.

Envelope: `{"type":"on_enable|on_disable|on_trigger|on_interaction|on_ui_call","settings":{...},"input":...}`.
Interações e RPC usam os dois novos callbacks; **on_event continua fora desta rodada**.
O dispatcher entrega somente a integrações remote enquanto isso, sem outro schema.

Host calls (identidade sempre implícita, checks atuais de capacidade e escopo):

```json
{"method":"asset.read","path":"templates/message.txt","offset":0,"length":32768}
{"method":"kv.put","key":"counter","value":42}
{"method":"kv.get","key":"counter"}
{"method":"kv.delete","key":"counter"}
{"method":"message.send","channel_id":"UUID","content":"Olá","embeds":[{"source_type":"app","app_surface_id":"result","title":"Resultado"}]}
{"method":"message.edit","channel_id":"UUID","message_id":"UUID","content":"Editado"}
{"method":"message.delete","channel_id":"UUID","message_id":"UUID"}
{"method":"log","content":"diagnóstico"}
{"method":"time.now"}
```

Asset retorna `result.data` em base64, `offset` e `length`; leituras parciais permitem
arquivos maiores até o teto de asset. KV exige `private_storage`: 256 chaves,
1 MiB agregado de chave + JSON normalizado, chave de 1–200 bytes, valor padrão até 16 KiB (configurável),
revisão incrementada em update (delete + put reinicia em 1). SQL serializa a quota.
`message.send` usa `CreatePluginMessage`, mesmo armazenamento/publicação de
mensagens, capacidade send e escopo atual; até um embed da própria superfície
`embed` com `post_app_embeds`. Humanos/webhooks e superfícies alheias são recusados.
Edit/delete só atingem mensagens próprias. Não há `external_author` arbitrário.

Whitelist WASI: fd_write (stdout/stderr descartados), random_get (entropia), args_get
+ args_sizes_get (JSON/runtime TinyGo, argumentos vazios) e clock_time_get com a
assinatura exata **mas implementação que sempre retorna ENOSYS**, sem relógio.
Hora real só por `time.now`. Sem env, preopens, filesystem ou sockets WASI.
Start section, imports não funcionais, `_start` e ABI incompatível são rejeitados.
Tabelas funcref têm máximo explícito até 4096; tipos/funções/globals/segmentos até 4096.

Uma execução por instalação, oito aguardando, quatro trabalhos globais, deadline
Wasm de 1 s com `WithCloseOnContextDone`. A fila do manager também tem até oito
aguardando e deadline de trigger de 2 s incluindo validação/transições. Payloads de
64 KiB, 32 host calls. Três falhas consecutivas abrem o circuito e o manager persiste
quarentena no banco. Reaprovação é explícita. Compilação serializada e cancelável;
cache LRU por módulo **e memória aprovada**, orçamento global estimado de 128 MiB
(`max(1 MiB,4*bytes)`), sem eviction de handles ativos. Não é teto de RSS.

Wasm embutido não oferece isolamento de SO. Host calls Go devem cooperar com
contexto; efeitos anteriores a trap/timeout permanecem confirmados. Idempotência
de mensagem só dura enquanto ela existir, como REST. Uma transição fica marcada como pendente no SQL e só é publicada ao terminar;
UI/trigger externos falham fechado enquanto isso. No rescan após término abrupto,
transição pendente entra em quarentena e exige reaprovação. Não se promete
atomicidade entre SQL, filesystem e efeitos já confirmados do guest.

## Exemplos e CI

`examples/plugins/reference` inclui SDK TinyGo, canal bundled, embed bundled, ícone
e template lido por asset.read; trigger `{}` publica o embed no setting autorizado.
Para exercitar KV/negações pelo exemplo: trigger `{"request":{"method":"kv.get","key":"counter"}}`.
`examples/plugins/bluemap` tem reactor mínimo (~12 KiB), ícone e setting `map_url`
para a superfície remota. Nenhuma lógica de mapa é executada no backend.

CI instala TinyGo fixado com checksum e compila ambos e o fixture adversarial;
triggers e Codecov permanecem iguais. Go tests executam os binários versionados
quando TinyGo não está instalado. Warning de triples wasip1/wasi conhecido do spike;
os binários são testados de verdade com wazero e servidor HTTP.

## Smoke HTTP reproduzível

Use um banco **novo e descartável**, com todas as migrations, e um binário compilado:

```bash
cd backend
go build -o /tmp/papo-plugin-server ./cmd
cd ..
goose -dir migrations postgres "$SMOKE_DATABASE_URL" up
python3 examples/plugins/smoke.py --server /tmp/papo-plugin-server --database-url "$SMOKE_DATABASE_URL"
```

O script inicia o servidor em uma pasta temporária, copia os dois exemplos, faz
rescan/preview/enable, verifica os canais em GET /channels, dispara o reference,
confere o embed, executa curl público e imprime os headers. Reinicia o processo,
confere KV, desabilita ambos, verifica que os canais sumiram e que a UI retorna
404. Encerra o servidor e remove apenas a pasta temporária criada pelo smoke.
O banco fica disponível para inspeção e deve ser removido pelo chamador.

### Smoke dos assets de UI bundled

As páginas `index.html` e `result.html` do plugin de referência compartilham CSS,
um módulo JS, uma imagem SVG, uma fonte TTF mínima própria do fixture e um JSON
lido por `fetch()`. O status visível e o título
`PAPO_RESOURCE_CHECK: CSS module image font fetch OK` só aparecem depois de o
módulo verificar CSS aplicado, imagem decodificada, fonte carregada e JSON lido.
O título permite observar o resultado por API nativa sem injetar scripts ou bridge.

Verificado em WPE 2.54.1 com sessão efêmera, backend local e Postgres 16 migrado
com goose: as duas páginas e todos os recursos retornaram 200, e ambas chegaram
ao título de sucesso. WebView2 não estava disponível neste host.

A origem explícita no CSP vem de `APPS_ORIGIN` (origem HTTP(S), sem caminho), ou
é extraída de `BASE_URL` quando a variável está vazia. Nunca vem dos headers
Host/Forwarded. O smoke HTTP também busca as duas páginas e cada asset real com
`Origin: null`, verifica MIME, ausência de credenciais e CORS em 200 e 304.
A execução HTTP não substitui a execução do módulo em uma WebView.


## Quotas administrativas de app_storage

Defaults globais: PLUGINS_STORAGE_MAX_KEYS=256,
PLUGINS_STORAGE_MAX_TOTAL_BYTES=1048576 e PLUGINS_STORAGE_MAX_VALUE_BYTES=16384.
Configuração inválida impede startup. Limites aceitos: 1–100000 chaves,
1 byte–1 GiB total, 1 byte–64 KiB por valor, e valor <= total. O transporte
papo-core-v1 conserva seu limite independente de 64 KiB por payload.

Preview/enable e PATCH /plugins/:id aceitam `storage_quota` com max_keys,
max_total_bytes e max_value_bytes opcionais. Ausência conserva o override atual;
zero explícito herda o default global daquele campo. Respostas mostram quota
efetiva em storage_quota e configuração persistida em storage_quota_overrides.
O consentimento assinado inclui ambas. Overrides não podem vir do manifest,
setting do guest ou RPC. PATCH exige manage_apps, como max_memory_mb.

A mutation KV resolve os limites dentro da transação, travando somente a linha
de uso da instalação (`app_storage_usage`, atualizada na mesma transação), com
JSONB normalizado e bytes UTF-8 das chaves; não soma todas as chaves a cada
escrita. A redução não apaga dados; novas escritas precisam caber e delete
continua disponível. Reaprovação, restart e hot reload preservam o override. O
CHECK físico de app_storage limita cada valor a 64 KiB, acima do teto
configurável por valor.
