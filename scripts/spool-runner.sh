#!/usr/bin/env bash
set -euo pipefail

SPOOL_DIR="${SPOOL_DIR:-/opt/go-pet/spool}"
IN_DIR="$SPOOL_DIR/in"
PROCESSING_DIR="$SPOOL_DIR/processing"
OUT_DIR="$SPOOL_DIR/out"
API_BASE_URL="${API_BASE_URL:-http://127.0.0.1:8080/api/v1}"
RUNNER_TOOL_TIMEOUT="${RUNNER_TOOL_TIMEOUT-300}"

if [[ ! "$RUNNER_TOOL_TIMEOUT" =~ ^[1-9][0-9]{0,4}$ ]] || (( RUNNER_TOOL_TIMEOUT > 86400 )); then
    echo "Error: RUNNER_TOOL_TIMEOUT must be an integer from 1 to 86400 seconds." >&2
    exit 1
fi

for cmd in jq python3 curl timeout flock mkdir mktemp mv rm sleep; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "Error: required tool '$cmd' is not installed." >&2
        exit 1
    fi
done
if [[ "$(timeout --version 2>/dev/null)" != 'timeout (GNU coreutils)'* ]]; then
    echo "Error: GNU coreutils timeout is required." >&2
    exit 1
fi

mkdir -p "$IN_DIR" "$PROCESSING_DIR" "$OUT_DIR"

# Never remove this file: competing runners must lock the same inode.
exec {lock_fd}>>"$SPOOL_DIR/.runner.lock"
if ! flock -n "$lock_fd"; then
    echo "Error: cannot acquire spool lock; another runner may be active." >&2
    exit 1
fi

send_status() {
    local job_id="$1"
    local status="$2"
    local json_body="$3"

    local http_code reason
    if http_code=$(curl -q -s --connect-timeout 3 --max-time 10 \
        -o /dev/null -w '%{http_code}' -X POST \
        -H "Content-Type: application/json" \
        -d "$json_body" \
        "$API_BASE_URL/jobs/$job_id/status" 2>/dev/null); then
        [[ "$http_code" == 204 ]] && return 0
        reason="expected HTTP 204"
    else
        reason="curl exit $?"
    fi
    printf 'Error: job %s callback %s unconfirmed (%s); check the database before manual recovery.\n' \
        "$job_id" "$status" "$reason" >&2
    return 1
}

run_tool() {
    local stderr_path="$1"
    shift
    # Bash may report SIGKILL of timeout itself; keep it out of the tool's stderr.
    # External tools and their descendants must not retain the runner's lock.
    { timeout --kill-after=1s "${RUNNER_TOOL_TIMEOUT}s" "$@" 2>"$stderr_path" {lock_fd}>&-; } 2>/dev/null
}

supports_action_version() {
    # jq 1.6 rounds JSON numbers to binary64; a fraction can compare equal to 1.
    python3 - "$1" 2>/dev/null <<'PY'
import json
import sys
from decimal import Decimal

with open(sys.argv[1], encoding="utf-8") as source:
    job = json.load(source, parse_int=Decimal, parse_float=Decimal)
version = job["action_version"] if "action_version" in job else Decimal(1)
sys.exit(not (isinstance(version, Decimal) and version == 1))
PY
}

validated_tool() {
    supports_action_version "$1" || return 1
    # The SQL validator remains authoritative; reject malformed spool arguments
    # before Bash converts them to strings or passes them to a CLI.
    jq -er '
        def matches($re): type == "string" and test($re);
        def nonblank: matches("[^[:space:]]");
        def http_url:
            type == "string"
            and (test("[[:space:][:cntrl:]\\\\]|%(?![0-9A-Fa-f]{2})") | not)
            and test("\\A(?i:https?)://([A-Za-z0-9._~%!$&\u0027()*+,;=:-]*@)?([A-Za-z0-9._~%!$&\u0027()*+,;=-]+|\\[[A-Fa-f0-9:.]*:[A-Fa-f0-9:.]*(%25([A-Za-z0-9._~-]|%[0-9A-Fa-f]{2})+)?\\])(:[0-9]*)?([/?#]|\\z)");
        .tool_name as $tool
        | .payload
        | select(type == "object" and .tool == $tool)
        | select(
            if $tool == "echo-test" then
                (has("message") | not) or (.message | nonblank)
            elif $tool == "yt-dlp" then
                .url | http_url
            elif $tool == "fabric" then
                (.text | nonblank) and
                ((has("pattern") | not) or (.pattern | matches("\\A[A-Za-z0-9_-]{1,128}\\z")))
            else false end
        ) | $tool
    ' "$1" 2>/dev/null
}

prepare_public() {
    local tool="$1" job_file="$2" dir="$3"
    case "$tool" in
        echo-test)
            jq '{"type":"echo-test","version":1,"data":{"echo":(.payload.message // "default echo")}}' "$job_file"
            ;;
        fabric|yt-dlp)
            local artifact="$dir/output.md"
            if [[ "$tool" == yt-dlp ]]; then artifact="$dir/result.json"; fi
            python3 - "$tool" "$artifact" <<'PY'
import json
import re
import sys
from decimal import Decimal
from pathlib import Path


def invalid_constant(value):
    raise ValueError("non-JSON constant: " + value)


def project_items(text):
    decoder = json.JSONDecoder(parse_constant=invalid_constant, parse_int=Decimal)
    whitespace = re.compile(r"[ \t\r\n]*")
    items = []
    offset = whitespace.match(text).end()
    while offset < len(text):
        obj, end = decoder.raw_decode(text, offset)
        if not isinstance(obj, dict):
            raise ValueError("expected metadata object")
        item = {key: obj[key] for key in ("title", "description")
                if isinstance(obj.get(key), str)}
        if not item:
            raise ValueError("expected title or description")
        items.append(item)
        offset = whitespace.match(text, end).end()
    if not items:
        raise ValueError("empty metadata stream")
    return items


# Decode the original bytes strictly and retain all line endings.
tool, artifact = sys.argv[1:]
text = Path(artifact).read_bytes().decode("utf-8")
if tool == "fabric":
    if not text:
        sys.exit(1)
    data = {"text": text}
else:
    data = {"items": project_items(text)}
result = json.dumps({"type": tool, "version": 1, "data": data}, ensure_ascii=False)
sys.stdout.buffer.write((result + "\n").encode("utf-8"))
PY
            ;;
        *) return 1 ;;
    esac
}

publish_public() {
    local tool="$1" job_file="$2" dir="$3" tmp
    tmp=$(mktemp -- "$dir/.public.XXXXXX") || return 1
    if prepare_public "$tool" "$job_file" "$dir" > "$tmp" &&
        mv -fT -- "$tmp" "$dir/public.json"; then
        return 0
    fi
    rm -f -- "$tmp"
    return 1
}

process_job() {
    local json_file="$1"
    local job_id
    local tool_name
    local job_out_dir
    local result_path

    if ! job_id=$(jq -ers '
        select(length == 1) | .[0] | select(type == "object") | .id
        | select(type == "string" and test("\\A[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}\\z"))
    ' "$json_file" 2>/dev/null); then
        printf 'Error: invalid job JSON or UUID; retained %q for inspection.\n' "$json_file" >&2
        return 1
    fi

    echo "Processing job $job_id..."

    if ! send_status "$job_id" "in_progress" '{"status":"in_progress"}'; then
        return 1
    fi

    job_out_dir="$OUT_DIR/$job_id"
    # Do not reuse the previous attempt's publication or follow an aliased job directory.
    if [[ -L "$job_out_dir" ]] || ! rm -f -- "$job_out_dir/public.json" 2>/dev/null; then
        printf 'Error: job %s cannot clear previous public result; check the spool and database before manual recovery.\n' \
            "$job_id" >&2
        return 1
    fi

    local exec_status=0
    local error_msg=""

    if tool_name=$(validated_tool "$json_file"); then
        mkdir -p "$job_out_dir"
        result_path="$job_out_dir/result.json"
    fi

    case "$tool_name" in
        echo-test)
            jq '{"status":"ok","echo":(.payload.message // "default echo")}' "$json_file" > "$result_path" 2>/dev/null || exec_status=$?
            ;;
        yt-dlp)
            if command -v yt-dlp &>/dev/null; then
                local url
                url=$(jq -r '.payload.url' "$json_file" 2>/dev/null)
                run_tool "$job_out_dir/stderr.log" yt-dlp --dump-json -- "$url" > "$result_path" || exec_status=$?
            else
                exec_status=1
                error_msg="yt-dlp CLI not found"
            fi
            ;;
        fabric)
            if command -v fabric &>/dev/null; then
                local pattern
                local -a pipeline_status
                pattern=$(jq -r '.payload.pattern // "summarize"' "$json_file" 2>/dev/null)
                if jq -j '.payload.text' "$json_file" 2>/dev/null |
                    run_tool "$job_out_dir/stderr.log" fabric --pattern "$pattern" > "$job_out_dir/output.md"; then
                    jq -n --arg p "$pattern" --arg path "$job_out_dir/output.md" \
                        '{"pattern":$p,"output_path":$path}' > "$result_path" || exec_status=$?
                else
                    pipeline_status=("${PIPESTATUS[@]}")
                    exec_status=${pipeline_status[1]}
                    if [[ $exec_status -eq 0 ]]; then
                        exec_status=${pipeline_status[0]}
                        error_msg="fabric: could not deliver payload.text"
                    fi
                fi
            else
                exec_status=1
                error_msg="fabric CLI not found"
            fi
            ;;
        *)
            exec_status=1
            error_msg="Invalid job tool, action version or payload"
            ;;
    esac

    if [[ $exec_status -eq 0 ]] &&
        ! publish_public "$tool_name" "$json_file" "$job_out_dir" 2>/dev/null; then
        exec_status=1
        error_msg="Could not prepare or publish public result"
    fi

    if [[ $exec_status -eq 0 ]]; then
        local body
        body=$(jq -n --arg path "/spool/out/$job_id/result.json" '{"status":"completed","result_path":$path}')
        send_status "$job_id" "completed" "$body" || return 1
        echo "Job $job_id completed successfully."
    else
        if [[ -z "$error_msg" ]]; then
            case "$exec_status" in
                124|137) error_msg="Tool $tool_name timed out or was killed (code $exec_status)" ;;
                *) error_msg="Tool $tool_name exited with code $exec_status" ;;
            esac
        fi
        local body
        body=$(jq -n --arg err "$error_msg" '{"status":"failed","error_msg":$err}')
        send_status "$job_id" "failed" "$body" || return 1
        echo "Job $job_id failed: $error_msg" >&2
    fi

    rm -f -- "$json_file"
}

run_once() {
    local file
    for file in "$IN_DIR"/*.json; do
        [[ -e "$file" || -L "$file" ]] || continue
        local processing_file="$PROCESSING_DIR/${file##*/}"

        if ! mv -T -n -- "$file" "$processing_file" ||
            [[ -e "$file" || -L "$file" ]]; then
            printf 'Error: cannot claim %q without replacing %q; check the spool and database.\n' \
                "$file" "$processing_file" >&2
            return 1
        fi
        process_job "$processing_file"
    done
}

if [[ "${1:-}" == "--loop" ]]; then
    echo "Starting spool-runner in daemon mode..."
    while true; do
        run_once
        sleep 2 {lock_fd}>&-
    done
else
    run_once
fi
