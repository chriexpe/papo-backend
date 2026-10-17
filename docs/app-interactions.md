# Interações, convites e ponte UI — PR 5

A implementação inclui §9.0 e §10.1–10.4 do plano v7, depois do merge C4.
Não inclui on_event, comandos ou SSE. Domínio não importa wazero.

## Mensagens e ações

`message.send` e `message.edit` aceitam `components: [[button, ...], ...]`:
até duas linhas de cinco botões. `id` é único por mensagem (1–64 caracteres),
`label` tem 1–80 caracteres, `style` é primary/secondary/danger e `disabled`
é booleano. `opens` opcional referencia somente superfície embed própria.
Esse botão abre diretamente o frame e pede launch; não chama on_interaction.
Uma mensagem pode conter só botões. Cards nativos continuam permitidos, junto
de no máximo um frame. O contrato de frames da C4 permanece size/load/display/poster.

Listagem e fixados hidratam os botões atuais; instalações indisponíveis os
retornam desabilitados. WS `message` inclui components quando existem;
`message_edit` de plugin inclui inclusive `[]` para remoção. Editar componentes
publica o evento para leitores atuais pelo mesmo pipeline das mensagens.
`app_invalidate` também alcança canais com mensagens que só tenham botões.

`[[user_actions]]` declara id, label e icon opcional de imagem existente em ui/.
O objeto app do canal expõe essas ações somente aos leitores. Elas só podem ser
executadas no canal app da própria instalação, sobre outro leitor desse canal.

## ABI e autoridade

Mantém exports alloc/free/invoke e o import papo_host_v1.call. Novos tipos de evento:

```json
{"type":"on_interaction","settings":{},"input":{"interaction_id":"uuid","kind":"user_action","user_id":"verificado","channel_id":"uuid","action_id":"convidar","target_user_id":"uuid"}}
{"type":"on_ui_call","settings":{},"input":{"user_id":"verificado","channel_id":"uuid","session_id":"uuid","method":"game.state","params":{},"invite":{"id":"uuid","inviter_id":"uuid","data":{}}}}
```

Interação de botão substitui action_id/target_user_id por message_id/component_id.
`invite` é opcional e vem do convite aceito pelo destinatário. Nunca recebe
identidade ou token fornecido dentro de params. Resposta segue `{ok,result,error}`.
O `result` de on_interaction volta sem alteração na resposta de `POST /interactions`
(`{"interaction_id":...,"result":...}`), somente para quem clicou; use-o para
feedback individual, e mensagens (`message.send`/`message.edit`) para o que todos
devem ver. Callbacks usam a mesma fila, memória, circuito e limites de host calls
do trigger. O prazo externo é 3 s configurável; o teto Wasm global
pode interromper antes. Timeout HTTP responde 504 com erro curto em português.
Efeitos já confirmados não são revertidos após falha posterior do guest.

Host call:

```json
{"method":"invite.send","to_user_id":"uuid","text":"Vamos jogar?","data":{"game_id":"uuid"}}
```

Exige send_invites e contexto humano de on_interaction **ou on_ui_call**.
Trigger, lifecycle e eventos não recebem esse contexto. Remetente e canal vêm do
servidor; o canal precisa ser app da instalação e ambos precisam lê-lo. Mute bloqueia
a criação. Texto: 500 caracteres; data: 8 KiB; validade padrão: 24 h. Notificação
app_invite e job da outbox são gravados na mesma transação. new_notification recebe
kind, app_installation_id, payload e expires_at; message_id fica null em convites.
Push conserva a outbox e inclui o tipo e a instalação, sem credenciais.

## HTTP

| Método | Rota | Autorização |
|---|---|---|
| POST | /interactions | Sessão humana; valida seletor e leitura; 200 com o `result` do plugin |
| POST | /notifications/:id/accept | Destinatário; retorna contexto canal/superfície |
| POST | /notifications/:id/decline | Destinatário |
| PATCH | /apps/:installation_id/user-settings | Sessão humana; invites_muted |
| POST | /apps/:installation_id/launch | Sessão humana; surface_id/channel_id e message_id/invite_id opcionais |
| POST | /apps/:installation_id/session | Ticket, sem cookies; CORS * |
| POST | /apps/:installation_id/rpc | Bearer UI, sem cookies; CORS * |
| OPTIONS | session e rpc | Preflight sem credenciais |

Decisões de convite são atômicas e repetíveis na mesma direção. Não é possível
aceitar convite expirado/de outro usuário, nem alterar decisão já tomada.
Aceitar não cria token automaticamente. Launch de embed exige referência presente
numa mensagem própria no canal (frame ou botão opens); superfície channel precisa
corresponder ao canal app. Convite exige aceitação e os mesmos vínculos.

## Tokens e revogação

Segredos opacos de 32 bytes crypto/rand, base64url, prefixos pat_/pas_.
Somente SHA-256 é persistido. A tabela app_ui_tokens distingue ticket/session,
com os mesmos vínculos: usuário, instalação, canal, superfície, mensagem/convite
opcionais, digest e geração de autorização. Não são JWTs ou credenciais Papo.

Ticket dura 60 s; uma atualização condicional garante consumo único concorrente.
Sessão começa com 30 min de inatividade, renovados após RPC válido, sem exceder
8 h desde a troca. RPC recusado não estende o prazo. Cada chamada revalida ACL,
banimento, digest e disponibilidade. Invalidação da instalação muda sua geração;
tokens antigos não voltam a funcionar. Um RPC recusado por perda de leitura
apaga os tokens daquele usuário na instalação, de modo que restaurar o acesso
depois não os reativa. Mudanças de ACL, roles ou banimento de outros usuários não
afetam sessões de quem continua lendo o canal.

Os limites por minuto (interações, lançamentos, RPC, convites, users.resolve)
ficam em memória, por chave, como os demais rate limits do servidor.

Rotina periódica remove lotes de tokens consumidos/expirados e convites expirados
(e seus pushes pendentes). Usa contexto cancelável, limites,
SKIP LOCKED e transação, e sua parada é aguardada no ciclo do servidor.

## Limites e SDK

Variáveis em backend/.env.sample, limites por minuto:

- PLUGINS_INTERACTIONS_PER_USER=30; PLUGINS_INTERACTIONS_PER_PLUGIN=120.
- PLUGINS_INVITES_PER_SENDER=10; PLUGINS_INVITES_PER_TARGET=5.
- PLUGINS_LAUNCH_PER_USER=60.
- PLUGINS_RPC_PER_SESSION=120; PLUGINS_RPC_PER_PLUGIN=600.
- PLUGINS_INTERACTION_TIMEOUT_MS=3000; PLUGINS_INVITE_TTL_SECONDS=86400;
  PLUGINS_CLEANUP_SECONDS=60.

Buckets SQL compartilham limites entre processos; excesso retorna 429 e
Retry-After: 60. O runtime embutido continua serializado por instalação e processo;
múltiplos servidores executando a mesma partida não têm exclusão distribuída.

`examples/plugin-sdk/papo-plugin-ui.js` troca e remove o fragmento, usa
credentials: omit e conserva o token somente em memória. connect({pollingMs:1500})
expõe rpc e poll; poll aceita intervalMs, não sobrepõe chamadas e cancela em pagehide.
Para UI remota ou APPS_ORIGIN separado, informe baseURL e installationId ao connect.
O build copia o helper para ui/. Os tipos TinyGo ficam em examples/plugin-sdk/sdk.go.

A demo reference declara a ação Convidar para jogar. Após convite/aceitação, ambos
abrem seu canal com tickets separados e jogam via RPC, com turno/revisão validados
no guest e estado em app_storage. A mensagem tem Revanche e Jogar (opens=result,
overlay). `smoke.py` mede confirmação HTTP e observação pelo segundo jogador no
próximo polling de 1500 ms; isso não mede renderização de WebView.

Compile com TINYGO=/caminho/tinygo bash examples/plugins/build.sh. A CI mantém seus
triggers e o comportamento de Codecov. Execute smoke.py contra banco descartável
novo, migrado pelo goose; não use banco de produção. Logs do smoke não exibem tokens.

Hot reload sem expansão também é aplicado na fronteira de uma chamada humana.
A nova geração invalida a sessão do digest anterior; a UI precisa de novo launch.
O contexto humano é retirado de on_enable/on_disable, inclusive quando o lifecycle
acontece dentro de um hot reload iniciado por interação/RPC.


## Perfis públicos e avatares

Os callbacks humanos incluem `input.user: {id,username,display_name,avatar_url}`;
user_action também inclui `target_user`. São perfis dos IDs verificados pelo host,
nunca de params. IDs legados permanecem. display_name usa nickname não vazio,
com fallback para username; avatar_url é `/users/<id>/avatar`, ou null sem avatar.

Host call `users.resolve {ids}` retorna `result: [perfil,...]`, até 50 IDs de entrada.
Ausentes, banidos e usuários sem leitura de nenhum canal app próprio ou escopo
atual são omitidos de maneira indistinguível. Outro canal público sem vínculo
não concede visibilidade. Duplicatas são eliminadas, preservando ordem. A consulta
seleciona somente esses quatro campos no SQL e reutiliza a ACL de leitura normal.
Não exige uma nova capacidade e não concede acesso a mensagens. O limite é
PLUGINS_USERS_RESOLVE_PER_PLUGIN=120 chamadas por minuto por instalação.

O jogo usa game.profiles → users.resolve para os dois participantes e carrega nomes
com textContent e avatares públicos com crossorigin=anonymous. O SDK fornece
avatarURL(profile) para resolver o caminho relativo ao servidor. O endpoint público
de avatar envia CORP cross-origin, CORS * sem credenciais e nosniff, inclusive com
CORS global ativo. Quando APPS_ORIGIN difere de BASE_URL, o CSP permite imagens de
BASE_URL/users/ sem abrir o restante da API. Falta de avatar mantém o nome visível.

Verificação nativa opcional: compile `examples/plugins/verify-avatars-wpe.c` com
`cc ... $(pkg-config --cflags --libs wpe-webkit-2.0 wpe-platform-headless-2.0)` e passe
`--avatar-browser /caminho/verificador` ao smoke.py. Ele cria tickets distintos para
index.html/result.html e os entrega ao processo por stdin, sem imprimir o segredo.
WPE usa sessão efêmera, sem cookie Papo ou scripts injetados, e confirma o título
PAPO_PLAYER_AVATARS_OK emitido pela própria página só após decode() e naturalWidth
positivos nos dois avatares reais. Isso verifica carregamento sob CSP sandbox opaco.
