import { connect } from './papo-plugin-ui.js';
const status = document.querySelector('#game-status');
const board = document.querySelector('#board');
let current;
let moving = false;
let profilesGame;
let loadingProfiles = false;
try {
  const app = await connect();
  const cells = Array.from({ length: 9 }, (_, cell) => {
    const button = document.createElement('button');
    button.setAttribute('aria-label', `Casa ${cell + 1}`);
    button.addEventListener('click', async () => {
      if (moving || !current) return;
      moving = true;
      const started = performance.now();
      try {
        render(await app.rpc('game.move', { cell, revision: current.game.revision }));
        document.querySelector('#latency').textContent = `Confirmação da jogada: ${(performance.now() - started).toFixed(1)} ms. Outro jogador: polling a cada ${app.pollingMs} ms.`;
      } catch (error) { status.textContent = error.message; }
      finally { moving = false; }
    });
    board.append(button);
    return button;
  });
  async function loadProfiles(gameId) {
    if (loadingProfiles || profilesGame === gameId) return;
    loadingProfiles = true;
    try {
      const profiles = await app.rpc('game.profiles');
      const players = document.querySelector('#players');
      players.replaceChildren();
      const images = profiles.map(profile => {
        const player = document.createElement('span');
        const image = document.createElement('img');
        image.alt = `Avatar de ${profile.display_name}`;
        image.width = image.height = 40;
        image.crossOrigin = 'anonymous';
        const url = app.avatarURL(profile);
        if (url) image.src = url;
        else image.hidden = true;
        const label = document.createElement('span');
        label.textContent = profile.display_name;
        player.append(image, label);
        players.append(player);
        return image;
      });
      profilesGame = gameId;
      const loaded = await Promise.allSettled(images.map(image => image.decode()));
      images.forEach((image, i) => { if (loaded[i].status !== 'fulfilled') image.hidden = true; });
      if (images.length === 2 && images.every(image => image.naturalWidth > 0)) {
        document.title = 'PAPO_PLAYER_AVATARS_OK';
      }
    } catch (error) { status.textContent = error.message; }
    finally { loadingProfiles = false; }
  }
  function render(value) {
    if (current && value.game.revision < current.game.revision) return;
    current = value;
    const game = value.game;
    loadProfiles(game.id);
    status.textContent = game.winner ? `Resultado: ${game.winner}` : `Vez de ${game.turn === 0 ? 'X' : 'O'}`;
    cells.forEach((button, i) => {
      button.textContent = game.board[i] || '·';
      button.disabled = !!game.winner || !!game.board[i] || game.players[game.turn] !== value.user_id;
    });
  }
  app.poll('game.state', {}, render, { onError: error => { status.textContent = error.message; } });
} catch (error) { status.textContent = error.message; }
