#!/usr/bin/env bash
# Build the assignment report PDF from report.html (requires Chrome/Chromium).
cd "$(dirname "$0")"
CHROME=$(command -v google-chrome || command -v chromium || command -v chromium-browser)
"$CHROME" --headless=new --disable-gpu --no-sandbox \
  --print-to-pdf=REPORT.pdf "file://$PWD/report.html"
echo "wrote $(dirname "$0")/REPORT.pdf"
