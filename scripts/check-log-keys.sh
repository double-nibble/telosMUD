#!/usr/bin/env bash
# check-log-keys.sh — #454 guard.
#
# Fails if a sensitive key name is used as a structured-log key in a logging call. The VALUES behind
# these keys are verbatim player text (a tell/say body, a mistyped link code) or credentials, and
# once container stdout is shipped into Loki (Observability 2) a log line is a durable, indexed,
# queryable archive. This guard makes "no raw input / secret in the logs" an enforced invariant, not
# a convention that erodes.
#
# The only legitimate sites log the raw input line, and do so ONLY behind the explicit
# TELOS_LOG_RAW_INPUT opt-in (separate from DEBUG). Those lines are annotated with `logkey-ok` and a
# reason, and are exempt. To add a new exemption, gate the value behind the opt-in and annotate it.
#
# Limitation: this matches single-line log calls (the codebase's universal style). A key split onto
# its own line from the log-method token is not detected; keep log calls single-line.
set -euo pipefail

# Sensitive slog KEYS. Anchored to the double-quoted key form so struct tags (`json:"body"`) and map
# literals (`"text":`) do not match — only a positional slog arg `"key", value` / `"key")` does.
keys='line|body|text|token|secret|assertion'

# A structured-log call on the line.
logcall='\.(Debug|Info|Warn|Error|Debugf|Infof|Warnf|Errorf|DebugContext|InfoContext|WarnContext|ErrorContext)\('

roots=(internal cmd)

hits=$(grep -rnE "$logcall" --include='*.go' "${roots[@]}" \
  | grep -vE '_test\.go:' \
  | grep -E "\"($keys)\"[,)]" \
  | grep -v 'logkey-ok' || true)

if [[ -n "$hits" ]]; then
  echo "check-log-keys: FAIL — a sensitive value is being used as a structured-log key (#454)."
  echo "Drop the key, or (for raw player input) gate it behind TELOS_LOG_RAW_INPUT and annotate"
  echo "the line with a trailing 'logkey-ok: <reason>' comment."
  echo
  echo "$hits"
  exit 1
fi

echo "check-log-keys: OK (no sensitive keys in structured-log calls)"
