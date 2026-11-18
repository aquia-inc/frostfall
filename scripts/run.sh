#!/usr/bin/env bash
# Builds the frostfall command line from the action's inputs and runs it.
# Called by action.yml with every input in an FF_* environment variable.
# Inputs only ever become quoted array elements, never part of a script body,
# so a crafted input cannot execute in the caller's runner. Written for
# bash 3.2 (macOS runners): no associative arrays, no ${var,,}, no mapfile.
set -euo pipefail

CR=$'\r'
LF=$'\n'

# escape_cmd TEXT: encode text for a workflow command message. Input values
# can contain newlines, and an unescaped one would start a new line that the
# runner parses as its own command (::set-output, ::stop-commands::).
escape_cmd() {
  local s="$1"
  s="${s//%/%25}"
  s="${s//$CR/%0D}"
  s="${s//$LF/%0A}"
  printf '%s' "$s"
}

# fail MESSAGE: report an input problem as a step error and stop before any
# scan runs.
fail() {
  printf '::error::%s\n' "$(escape_cmd "$1")"
  exit 2
}

# check_bool NAME VALUE: accept only "", "true" or "false".
check_bool() {
  case "$2" in
    ""|true|false) ;;
    *) fail "input '$1' must be \"true\", \"false\" or empty, got \"$2\"" ;;
  esac
}

# check_line NAME VALUE: free-text values are single lines. A line break is
# never meaningful in a path, URL, profile name or regex, and refusing it keeps
# a stray YAML block-scalar newline from silently changing what a regex
# matches. The value is not echoed.
check_line() {
  case "$2" in
    *"$CR"*|*"$LF"*) fail "input '$1' must not contain line breaks" ;;
  esac
}

# An empty format falls back to the action's default. An empty config passes
# no --config at all, so the CLI looks for .frostfall.yml and then frostfall.yml.
: "${FF_FORMAT:=html}"

FF_CONFIG="${FF_CONFIG:-}"
FF_SERVE="${FF_SERVE:-}"
FF_BASE_URL="${FF_BASE_URL:-}"
FF_BASELINE="${FF_BASELINE:-}"
FF_OUTPUT="${FF_OUTPUT:-}"
FF_GH_ISSUES="${FF_GH_ISSUES:-}"
FF_PROFILE="${FF_PROFILE:-}"
FF_ID="${FF_ID:-}"
FF_PATH="${FF_PATH:-}"
FF_DISCOVER="${FF_DISCOVER:-}"
FF_SCREENSHOTS="${FF_SCREENSHOTS:-}"

check_line config "$FF_CONFIG"
check_line serve "$FF_SERVE"
check_line base-url "$FF_BASE_URL"
check_line baseline "$FF_BASELINE"
check_line format "$FF_FORMAT"
check_line output "$FF_OUTPUT"
check_line profile "$FF_PROFILE"
check_line id "$FF_ID"
check_line path "$FF_PATH"
check_bool gh-issues "$FF_GH_ISSUES"
check_bool discover "$FF_DISCOVER"
check_bool screenshots "$FF_SCREENSHOTS"

ARGS=(--format "$FF_FORMAT")
[ -n "$FF_CONFIG" ]   && ARGS+=(--config "$FF_CONFIG")
[ -n "$FF_SERVE" ]    && ARGS+=(--serve "$FF_SERVE")
[ -n "$FF_BASE_URL" ] && ARGS+=(--base-url "$FF_BASE_URL")
[ -n "$FF_BASELINE" ] && ARGS+=(--baseline "$FF_BASELINE")
# No --output when empty: the CLI picks the default file for the format.
[ -n "$FF_OUTPUT" ]   && ARGS+=(--output "$FF_OUTPUT")
# No --profile when empty: the automatic ci profile in Actions still applies.
[ -n "$FF_PROFILE" ]  && ARGS+=(--profile "$FF_PROFILE")
[ -n "$FF_ID" ]       && ARGS+=(--id "$FF_ID")
[ -n "$FF_PATH" ]     && ARGS+=(--path "$FF_PATH")
[ "$FF_DISCOVER" = "true" ]    && ARGS+=(--discover)
[ "$FF_GH_ISSUES" = "true" ]   && ARGS+=(--gh-issues)
# Empty leaves the CLI default (screenshots on in Actions).
[ "$FF_SCREENSHOTS" = "true" ]  && ARGS+=(--screenshots)
[ "$FF_SCREENSHOTS" = "false" ] && ARGS+=(--no-screenshots)

# The last command's status is the step's status.
frostfall "${ARGS[@]}"
