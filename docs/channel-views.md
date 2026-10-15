# Views passivas de canal (BlueMap)

Uma instalação `view` não recebe token nem executa código no backend. Um gestor
com `manage_apps` concede `attach_views`, adiciona os canais ao escopo e cadastra
uma URL HTTPS pública para cada canal. O gestor também precisa de `read_channel`
no destino. Canais de texto e voz são aceitos; DMs e categorias são recusados.

Exemplo usando uma sessão humana já autenticada em `cookies.txt`:

```sh
curl -b cookies.txt -H 'Content-Type: application/json' "$PAPO_URL/apps" \
  -d '{"app_key":"bluemap","kind":"view","name":"Mapa","capabilities":{"attach_views":true}}'
# Copiar o id retornado para APP_ID e escolher CHANNEL_ID.
curl -b cookies.txt -X PUT "$PAPO_URL/apps/$APP_ID/channels/$CHANNEL_ID"
curl -b cookies.txt -H 'Content-Type: application/json' \
  "$PAPO_URL/channels/$CHANNEL_ID/views" \
  -d "{\"app_installation_id\":\"$APP_ID\",\"url\":\"https://map.example.org/\",\"position\":0}"
curl -b cookies.txt "$PAPO_URL/channels"
```

Substitua `map.example.org` pelo host público real do seu BlueMap. O backend
rejeita localhost, nomes locais, redes privadas/reservadas, falhas de DNS,
portas não padrão, userinfo e o host configurado em `BASE_URL`, além de URLs
acima de 2.048 caracteres. Nenhum conteúdo é baixado ou servido pelo backend.

`GET /channels` acrescenta `views` somente para leitores, omitindo o campo se
vazio ou sem leitura. Respostas de criação, edição e mudança de posição de canais
seguem a mesma regra. A lista é ordenada por `position`, `created_at`, `id`;
empates de posição são permitidos. `GET /channels/:channel_id/views` é uma
listagem de gestão e também exige `manage_apps`.

`PUT /channels/:channel_id/views/:view_id` substitui `url` e `position` (padrão 0)
sem mover a view para outra instalação/canal. `DELETE` no mesmo caminho remove
a view. Remover o escopo, revogar a instalação ou excluir o canal apaga as views
por cascata. Retirar `attach_views` esconde as views enquanto a capacidade estiver
desabilitada; o gestor pode removê-las mesmo assim. Após anexar, editar ou remover uma view, remover seu escopo ou revogar a
instalação, os leitores atuais recebem o evento existente `channel_update`, com
`channel_id`, `name`, `position`, `topic` e `views`. A lista `views` substitui
integralmente a lista local; `[]` limpa a última aba. Outros updates sem o campo
`views` preservam a lista local. Após desconexão, consultar `GET /channels` para
reconciliar o estado. Não há novo tipo de evento WebSocket.

O frontend companheiro deve apresentar `Chat | Mapa` com WebView exclusiva,
perfil não persistente, sem credenciais Papo, bridge, scripts injetados ou
callbacks nativos. Navegação fica restrita à origem da view. Popups, downloads,
esquemas externos e permissões são negados ou abertos no navegador externo.
Se o site estiver inacessível, recusar framing ou a plataforma não conseguir
garantir o isolamento, mostrar **Abrir no navegador**. Validar a URL no cadastro
não garante segurança de DNS/redirects futuros. O contrato completo está no
schema `ChannelAppView` de `openapi.yml` (plano v4, §9.1 e §11).

A implementação e os testes de plataforma do frontend pertencem a uma PR própria;
este backend não implementa a aba nem fornece proxy, tokens ou SDK para views.
