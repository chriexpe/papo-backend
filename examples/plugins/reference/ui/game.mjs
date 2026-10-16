import { connect } from './papo-plugin-ui.js';
const status = document.querySelector('#game-status');
const board = document.querySelector('#board');
let current;
let moving = false;
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
  function render(value) {
    if (current && value.game.revision < current.game.revision) return;
    current = value;
    const game = value.game;
    status.textContent = game.winner ? `Resultado: ${game.winner}` : `Vez de ${game.turn === 0 ? 'X' : 'O'}`;
    cells.forEach((button, i) => {
      button.textContent = game.board[i] || '·';
      button.disabled = !!game.winner || !!game.board[i] || game.players[game.turn] !== value.user_id;
    });
  }
  app.poll('game.state', {}, render, { onError: error => { status.textContent = error.message; } });
} catch (error) { status.textContent = error.message; }
