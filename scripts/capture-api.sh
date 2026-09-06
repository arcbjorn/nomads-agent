#!/usr/bin/env bash
# Re-capture the Nomads.com API surface after a frontend change.
#
# Writes redacted output only: no cookies, keys or login hashes. Review
# anything before committing it.
set -euo pipefail

OUT="${1:-./captured}"
mkdir -p "$OUT"
UA='Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/131.0.0.0 Safari/537.36'

echo "==> Official API self-description"
curl -sS -A "$UA" https://nomads.com/api | tee "$OUT/api-index.json"

echo
echo "==> Machine-readable docs"
curl -sS -A "$UA" https://nomads.com/llms.txt -o "$OUT/llms.txt"
echo "saved $OUT/llms.txt"

echo
echo "==> Official MCP tool schemas"
curl -sS -X POST https://nomads.com/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' \
  | tee "$OUT/mcp-tools.json" >/dev/null
echo "saved $OUT/mcp-tools.json"

echo
echo "==> First-party frontend bundles"
for f in profile.js global.js; do
  curl -sS -A "$UA" "https://nomads.com/$f" -o "$OUT/$f"
  echo "saved $OUT/$f"
done

echo
echo "==> Call sites in the first-party bundles"
grep -n 'user/api' "$OUT/profile.js" "$OUT/global.js" || true

echo
echo "==> Actions referenced"
grep -ohE "['\"]action['\"]\s*:\s*['\"][a-z_0-9-]+['\"]" "$OUT"/*.js \
  | sort -u || true

cat <<'NOTE'

Next steps:
  1. Open an authenticated profile and watch DevTools > Network (Fetch/XHR)
     while editing a bio, toggling a tag, and adding/editing/deleting a trip.
  2. The profile edit modal's handler is an inline <script> on /@<handle>,
     not in profile.js. Grep the page source for 'saveField'.
  3. Reconcile findings with docs/api-observations.md and internal/client/.

Nothing here is redacted automatically. Do not commit captured pages: they can
contain session cookies and personal data.
NOTE
