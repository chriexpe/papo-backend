try {
  const data = await fetch('./data.json').then(response => {
    if (!response.ok) throw new Error(`JSON: ${response.status}`);
    return response.json();
  });
  await document.fonts.load('16px "Papo Fixture"', 'Papo');
  await document.querySelector('img').decode();
  if (getComputedStyle(document.body).getPropertyValue('--fixture-css').trim() !== 'loaded') {
    throw new Error('CSS não carregado');
  }
  document.querySelector('#resource-status').textContent = data.message;
  document.title = 'PAPO_RESOURCE_CHECK: CSS module image font fetch OK';
  console.info(document.title);
} catch (error) {
  document.querySelector('#resource-status').textContent = `Falha: ${error.message}`;
  console.error('PAPO_RESOURCE_CHECK:', error);
}
