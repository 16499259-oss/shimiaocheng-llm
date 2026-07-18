// Frontend contract test for renderExpiryRow() in web/user/index.html.
//
// renderExpiryRow is the function the self-service /user/ panel uses to render
// the account-expiry stat row (the visible half of the user-expiry-display
// feature). It is embedded in index.html, so instead of copying it we extract
// the real source verbatim and execute it, asserting on the stable status
// tokens it emits (永久 / 已过期 / 天后到期). Date-label formatting is left
// unchecked because it depends on the host ICU data and is not part of the
// contract.
//
// Run:  node web/user/quota_expiry.test.mjs
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const __dirname = dirname(fileURLToPath(import.meta.url));
const html = readFileSync(join(__dirname, 'index.html'), 'utf8');

// Pull the function source verbatim so we test the shipped implementation.
const m = html.match(/function renderExpiryRow\(expiresAt\) \{[\s\S]*?\n\}/);
if (!m) {
  console.error('FAIL: could not locate renderExpiryRow in index.html');
  process.exit(1);
}
// eslint-disable-next-line no-eval
const renderExpiryRow = eval('(' + m[0] + ')');

let failures = 0;
function check(name, actual, mustContain, mustNotContain) {
  const problems = [];
  for (const s of mustContain) {
    if (!actual.includes(s)) problems.push(`missing ${JSON.stringify(s)}`);
  }
  for (const s of mustNotContain) {
    if (actual.includes(s)) problems.push(`unexpected ${JSON.stringify(s)}`);
  }
  if (problems.length) {
    failures++;
    console.error(`FAIL ${name}:\n  got: ${actual}\n  ${problems.join('; ')}`);
  } else {
    console.log(`PASS ${name}`);
  }
}

// 1. Permanent (empty) -> 永久, never the expired/near markers.
check('empty-permanent', renderExpiryRow(''), ['到期时间', '永久'], ['已过期', '天后到期']);

// 2. Defensive: null/undefined must also render as 永久 (API always sends a
//    string, but the guard must not throw if it ever doesn't).
check('null-permanent', renderExpiryRow(null), ['到期时间', '永久'], ['已过期', '天后到期']);
check('undefined-permanent', renderExpiryRow(undefined), ['到期时间', '永久'], ['已过期', '天后到期']);

// 3. Far future -> plain date label, no expired/near/permanent markers.
check('far-future', renderExpiryRow('2099-12-31T23:59:59Z'), ['到期时间'], ['已过期', '天后到期', '永久']);

// 4. Past date -> 已过期 flag, no near/permanent markers.
check('past-expired', renderExpiryRow('2020-01-01T00:00:00Z'), ['到期时间', '已过期'], ['天后到期', '永久']);

// 5. Unparseable -> raw string shown as fallback, no flags.
check('invalid-raw', renderExpiryRow('not-a-real-date'), ['到期时间', 'not-a-real-date'], ['已过期', '天后到期', '永久']);

// 6. Near expiry (6 days out) -> 天后到期 marker.
const soon = new Date(Date.now() + 6 * 86400000).toISOString();
check('near-expiry', renderExpiryRow(soon), ['到期时间', '天后到期'], ['已过期', '永久']);

// 7. 8 days out -> normal row, no near marker.
const later = new Date(Date.now() + 8 * 86400000).toISOString();
check('normal-future', renderExpiryRow(later), ['到期时间'], ['已过期', '天后到期', '永久']);

if (failures > 0) {
  console.error(`\n${failures} frontend expiry test(s) failed`);
  process.exit(1);
}
console.log('\nAll frontend expiry tests passed');
