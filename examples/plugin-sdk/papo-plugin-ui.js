/** Ponte confinada ao próprio plugin. Nunca lê cookies ou armazenamento do navegador. */
export async function connect({ baseURL, installationId, pollingMs = 1500 } = {}) {
  const match = location.pathname.match(/^\/apps\/([^/]+)\/ui\//);
  installationId ??= match?.[1];
  baseURL ??= location.origin;
  if (!installationId) throw new Error('Instalação do app ausente');
  const endpoint = `${baseURL.replace(/\/$/, '')}/apps/${encodeURIComponent(installationId)}`;
  const fragment = new URLSearchParams(location.hash.slice(1));
  const ticket = fragment.get('papo_ticket');
  if (!ticket) throw new Error('Abra esta superfície pelo Papo para iniciar a sessão');
  // O sandbox pode negar history.replaceState; o fragmento continua fora de requests/referrers.
  fragment.delete('papo_ticket');
  try { history.replaceState(null, '', location.pathname + location.search + (fragment.size ? `#${fragment}` : '')); }
  catch { location.hash = fragment.toString(); }
  async function post(path, body, token, signal) {
    const response = await fetch(endpoint + path, {
      method: 'POST', credentials: 'omit', cache: 'no-store', signal,
      headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      body: JSON.stringify(body),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.detail ?? result.title ?? 'Chamada do app recusada');
    return result;
  }
  const session = await post('/session', { ticket });
  const rpc = async (method, params = {}, signal) => {
    const response = await post('/rpc', { method, params }, session.token, signal);
    if (!response.ok) throw new Error(response.error?.detail ?? 'O plugin recusou a chamada');
    return response.result;
  };
  /** Polling sem chamadas sobrepostas; o retorno encerra timer e chamada em curso. */
  function poll(method, params, onValue, { intervalMs = pollingMs, onError = console.error } = {}) {
    if (!Number.isFinite(intervalMs) || intervalMs < 100) throw new Error('Intervalo de polling inválido');
    const controller = new AbortController();
    let timer;
    async function tick() {
      try { onValue(await rpc(method, params, controller.signal)); }
      catch (error) { if (!controller.signal.aborted) onError(error); }
      if (!controller.signal.aborted) timer = setTimeout(tick, intervalMs);
    }
    tick();
    const stop = () => { controller.abort(); clearTimeout(timer); };
    addEventListener('pagehide', stop, { once: true });
    return stop;
  }
  // Perfis usam caminho relativo ao servidor, mesmo se a página estiver em outro diretório.
  // Retorna null quando o usuário não tem avatar.
  function avatarURL(user) {
    if (user.avatar_url == null) return null;
    if (!/^\/users\/[0-9a-f-]{36}\/avatar$/i.test(user.avatar_url)) throw new Error('Avatar de usuário inválido');
    return new URL(user.avatar_url, baseURL).href;
  }
  return { rpc, poll, avatarURL, sessionId: session.session_id, pollingMs };
}
