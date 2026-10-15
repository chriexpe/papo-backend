# Plugins Wasm — papo-core-v1

A entrega usa `feat/channel-views` (PR #18, `2810c6b`) sobre `origin/apps-platform`.
A branch de integração ainda não contém PR 3: execução somente sob demanda,
sem callbacks, segundo a condição do plano. Nenhuma segunda estrutura de eventos.

## Artefato e autorização

Crie uma instalação `kind: wasm` (sem token), conceda `private_storage` para KV,
`send_messages` para envio e adicione os canais permitidos. JWT + `manage_apps`
protege `PUT /apps/:app_id/artifact` (bytes `application/wasm`) e
`POST /apps/:app_id/trigger` (JSON). O guest não escolhe sua identidade.
Cada host call recarrega instalação/capacidades; mensagens usam `CreateAppMessage`
e a mesma função `authorizeApp` do REST, incluindo tipo de canal e escopo atual.
KV tem namespace implícito e exclusivo da instalação, sem escopo de canal.

O upload compila e valida sem instanciar nem executar. Rejeita start section,
imports de memória/tabela/global, `_start`, falta de memória/exports e assinaturas
incompatíveis. SHA-256 dos bytes fica em `artifact_digest`; `artifact_sha` referencia
a mídia existente pelo HMAC-SHA-256, seguindo seu armazenamento atômico/deduplicado.
Antes de executar, o servidor relê o blob com limite e confere o SHA-256.
O GC reconhece artefatos e avatares das instalações. Não existe download público de Wasm.

## Contrato binário

Core Wasm; offsets/tamanhos unsigned i32; nenhuma referência ao endereço do host:

```text
memory: memória exportada
_initialize() -> ()
alloc(len: i32) -> ptr: i32
free(ptr: i32) -> ()
invoke(ptr: i32, len: i32) -> i64
  ptr_out = retorno >> 32; len_out = retorno & 0xffffffff
papo_host_v1.call(req_ptr:i32, req_len:i32, out_ptr:i32, out_cap:i32) -> len:i32
```

`_initialize` roda uma vez na nova instância. Não há estado persistente em globals.
`alloc` devolve buffer de tamanho solicitado e ponteiro não zero. O host copia
entrada e saída; o guest transfere ownership dos dois buffers para `free(ptr)`.
Entrada e saída não devem se sobrepor; saída pode reutilizar exatamente a entrada.
Buffers são válidos até free. Todos os caminhos fecham a instância, inclusive
falha de alloc, offsets inválidos, trap, timeout ou free defeituoso. Não se tenta
reentrar em uma instância fechada para liberar memória.

Entrada/saída são JSON UTF-8, sem NUL terminador. A saída exige `ok` booleano.
A importação lê um request JSON, devolve um envelope e escreve somente em um
scratch buffer pertencente ao guest. Retorna tamanho necessário sem escrever
quando out_cap é insuficiente; a operação **já pode ter produzido efeitos**.
Não repita automaticamente uma escrita; reserve 64 KiB ou use chave idempotente.
Respostas acima de 64 KiB e chamadas acima do teto encerram aquela instância.

Tabelas de funções: no máximo uma, funcref, máximo explícito de 4096 entradas;
contagens de tipos/funções/globals/segmentos até 4096 antes da compilação.

Imports permitidos, com assinaturas verificadas: apenas `papo_host_v1.call`,
`wasi_snapshot_preview1.fd_write(i32,i32,i32,i32)->i32` (panic/runtime TinyGo;
stdout/stderr descartados) e `random_get(i32,i32)->i32` (runtime TinyGo e entropia).
O módulo WASI padrão é instanciado, mas qualquer outro import é rejeitado.
Sem args/env configurados, preopens, stdin, filesystem ou rede para o guest.

## Host calls

```json
{"method":"kv.put","key":"counter","value":42}
{"method":"kv.get","key":"counter"}
{"method":"kv.delete","key":"counter"}
{"method":"message.send","channel_id":"UUID","content":"Olá","external_message_id":"op-1"}
```

Sucesso: `{"ok":true,"result":...}`. KV retorna `value` e `revision`; get ausente
retorna `not_found`, delete ausente é idempotente. Revisão incrementa em update;
após delete + put começa novamente em 1. Erros: `forbidden`, `invalid_param`,
`not_found`, `quota_exceeded`, `internal`. Não se expõem detalhes do SQL.
O exemplo encaminha cada entrada como um host call; outros guests podem compor chamadas.

## Limites e efeitos parciais

- Upload: 2 MiB; memória linear: 256 páginas (16 MiB).
- Input, output, request/response de host call: 64 KiB; 32 calls por invocação.
- Uma invocação por instalação; oito aguardando; quatro trabalhos globais.
- Prazo de 1 segundo incluindo espera e host calls; `WithCloseOnContextDone(true)`.
- Compilação serializada, admissão global e espera cancelável. Cache LRU por digest,
  orçamento global de 64 MiB em unidades estimadas `max(1 MiB, 16 * bytes do módulo)`.
  No máximo 64 módulos mínimos retidos. Handles ativos não sofrem eviction.
  O wazero não expõe o tamanho do código compilado: **não é um teto de RSS**.
- No máximo 1024 instalações com execução/falhas retidas. Saudáveis ociosas saem
  do registro; três falhas consecutivas abrem o circuito. Upload validado reseta;
  reiniciar o processo também limpa a quarentena (estado operacional em memória).
- KV: até 256 chaves, 1 MiB agregado de chave + JSONB normalizado; chave de
  1–200 bytes, valor de até 16 KiB antes/depois de normalização. A quota serializa
  no lock SQL da instalação e funciona entre processos. Dados sobrevivem ao restart.

Falhas preservam efeitos já confirmados: não existe transação para o invoke inteiro.
Propaga-se contexto para SQL e serviços; host calls Go devem cooperar com cancelamento.
A chave de mensagem deduplica somente enquanto a mensagem existe, como REST/webhooks.
O guest é código não confiável no processo do backend; esta interface não oferece
isolamento do sistema operacional. `PluginHost` preserva a fronteira para worker futuro.

## Build e teste

TinyGo **0.43.0** (LLVM 22.1.4), wazero **v1.12.0**. Reactor:

```bash
TINYGO=/caminho/tinygo/bin/tinygo examples/wasm-reference/build.sh
cd backend
go test -count=1 -race ./internal/wasmhost ./internal/plugins
TEST_DATABASE_URL='postgres://papo:papo123@127.0.0.1:5432/papo_test?sslmode=disable' go test -count=1 ./...
```

O script também compila o fixture adversarial (fontes em `testdata/adversarial`).
Os binários pequenos são versionados para permitir os testes Go sem toolchain TinyGo.
O CI instala a versão fixada com SHA-256 verificado e recompila antes do build/test;
os triggers e o passo Codecov existentes são preservados. O warning TinyGo de
triples wasip1/wasi também apareceu no spike; o teste real valida o artefato gerado.
