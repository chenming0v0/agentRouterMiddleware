import { readFile, readdir, rm } from 'node:fs/promises';

// Prune only AFTER a successful build: the user preview serves dist directly.
// Keep the current entry points, their font URLs, all public assets and .gitkeep.
const dist = new URL('../dist/', import.meta.url);
const html = await readFile(new URL('index.html', dist), 'utf8');
const references = text => [...text.matchAll(/\/assets\/([^\s"'()<>?#]+)/g)].map(match => match[1]);
const used = new Set(references(html));
for (const filename of [...used]) {
  if (!/\.(css|js)$/.test(filename)) continue;
  for (const asset of references(await readFile(new URL(`assets/${filename}`, dist), 'utf8'))) used.add(asset);
}
for (const filename of await readdir(new URL('assets/', dist))) {
  if (!/^(?:index-[\w-]+\.(?:js|css)|noto-sans-sc-[\w-]+\.woff2)$/.test(filename) || used.has(filename)) continue;
  await rm(new URL(`assets/${filename}`, dist));
}
