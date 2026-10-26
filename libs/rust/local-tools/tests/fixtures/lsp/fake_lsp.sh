#!/bin/sh
# A tiny language server for the tests (#1236): LSP framing over stdio
# (Content-Length headers) with scripted diagnostics, so no interpreter or
# build is needed. Every document it is given (didOpen, didChange) gets a
# publishDiagnostics for its version:
#   text holding OLD_BROKEN      an error "old error" (E0001), line 1
#   text holding UNDEFINED_NAME  an error "cannot find value `foo` in this
#                                scope" (E0425), line 3
# With `silent` as its first argument it never publishes. It appends what
# it receives ("<method> <uri>") to $TMPDIR/received.log, and on the first
# didOpen tries to write next to the document and to read it, and, with `probe <path>
# <port>` arguments, to read <path> and reach 127.0.0.1:<port>, logging
# write:ok|denied, readdoc:ok|denied, read:ok|denied and
# connect:ok|refused.
mode=$1
probe_path=$3
probe_port=$4
probed=false
log="${TMPDIR:-/tmp}/received.log"

send() {
  printf 'Content-Length: %s\r\n\r\n%s' "${#1}" "$1"
}

while :; do
  length=
  while IFS= read -r line; do
    line=$(printf '%s' "$line" | tr -d '\r')
    [ -z "$line" ] && break
    case "$line" in
      Content-Length:*) length=$(printf '%s' "$line" | sed 's/^Content-Length: *//') ;;
    esac
  done
  [ -z "$length" ] && exit 0
  body=$(dd bs=1 count="$length" 2>/dev/null)
  # Any key order: a workspace build with serde_json's preserve_order
  # (feature unification) sends "jsonrpc" before "id".
  id=$(printf '%s' "$body" | sed -n 's/^{.*"id":\([0-9][0-9]*\)[,}].*/\1/p')
  method=$(printf '%s' "$body" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  uri=$(printf '%s' "$body" | sed -n 's/.*"uri":"\([^"]*\)".*/\1/p')
  printf '%s %s\n' "$method" "$uri" >>"$log" 2>/dev/null
  case "$method" in
    initialize)
      send "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"capabilities\":{\"textDocumentSync\":1}}}"
      ;;
    shutdown)
      send "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":null}"
      ;;
    exit)
      exit 0
      ;;
    textDocument/didOpen|textDocument/didChange)
      if [ "$method" = textDocument/didOpen ] && [ "$probed" = false ]; then
        probed=true
        file=${uri#file://}
        if (: >"$file.written") 2>/dev/null; then echo "write:ok" >>"$log"; else echo "write:denied" >>"$log"; fi
        if cat "$file" >/dev/null 2>&1; then echo "readdoc:ok" >>"$log"; else echo "readdoc:denied" >>"$log"; fi
        if [ -n "$probe_path" ]; then
          if cat "$probe_path" >/dev/null 2>&1; then echo "read:ok" >>"$log"; else echo "read:denied" >>"$log"; fi
        fi
        if [ -n "$probe_port" ]; then
          if nc -z -w 2 127.0.0.1 "$probe_port" >/dev/null 2>&1; then echo "connect:ok" >>"$log"; else echo "connect:refused" >>"$log"; fi
        fi
      fi
      [ "$mode" = silent ] && continue
      version=$(printf '%s' "$body" | sed -n 's/.*"version":\([0-9][0-9]*\).*/\1/p')
      diags=
      case "$body" in
        *OLD_BROKEN*) diags='{"range":{"start":{"line":0,"character":4},"end":{"line":0,"character":9}},"severity":1,"code":"E0001","source":"fake","message":"old error"}' ;;
      esac
      case "$body" in
        *UNDEFINED_NAME*)
          [ -n "$diags" ] && diags="$diags,"
          diags="$diags"'{"range":{"start":{"line":2,"character":8},"end":{"line":2,"character":11}},"severity":1,"code":"E0425","source":"fake","message":"cannot find value `foo` in this scope"}'
          ;;
      esac
      send "{\"jsonrpc\":\"2.0\",\"method\":\"textDocument/publishDiagnostics\",\"params\":{\"uri\":\"$uri\",\"version\":$version,\"diagnostics\":[$diags]}}"
      ;;
  esac
done
