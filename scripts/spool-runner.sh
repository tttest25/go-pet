#!/usr/bin/env bash
set -euo pipefail

SPOOL_DIR="${SPOOL_DIR:-/opt/go-pet/spool}"
IN_DIR="$SPOOL_DIR/in"
PROCESSING_DIR="$SPOOL_DIR/processing"
OUT_DIR="$SPOOL_DIR/out"
API_BASE_URL="${API_BASE_URL:-http://127.0.0.1:8080/api/v1}"

for cmd in jq curl; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "Error: required tool '$cmd' is not installed." >&2
        exit 1
    fi
done

mkdir -p "$IN_DIR" "$PROCESSING_DIR" "$OUT_DIR"

send_status() {
    local job_id="$1"
    local status="$2"
    local json_body="$3"

    local response
    local http_code

    response=$(curl -s -w "\n%{http_code}" -X POST \
        -H "Content-Type: application/json" \
        -d "$json_body" \
        "$API_BASE_URL/jobs/$job_id/status")

    http_code=$(echo "$response" | tail -n1)
    
    if [[ "$http_code" -ge 400 ]]; then
        echo "Warning: failed to update job $job_id status $status (HTTP $http_code)" >&2
        return 1
    fi
}

process_job() {
    local json_file="$1"
    local job_id
    local tool_name
    local job_out_dir
    local result_path

    job_id=$(jq -r '.id' "$json_file")
    tool_name=$(jq -r '.tool_name' "$json_file")

    if [[ -z "$job_id" || "$job_id" == "null" ]]; then
        echo "Error: invalid job JSON file (missing id): $json_file" >&2
        rm -f "$json_file"
        return 1
    fi

    echo "Processing job $job_id (tool: $tool_name)..."

    if ! send_status "$job_id" "in_progress" '{"status":"in_progress"}'; then
        echo "Failed to mark job $job_id as in_progress, skipping." >&2
        return 1
    fi

    job_out_dir="$OUT_DIR/$job_id"
    mkdir -p "$job_out_dir"

    local exec_status=0
    local error_msg=""

    case "$tool_name" in
        echo-test)
            result_path="$job_out_dir/result.json"
            local message
            message=$(jq -r '.payload.message // "default echo"' "$json_file")
            jq -n --arg msg "$message" '{"status":"ok","echo":$msg}' > "$result_path"
            ;;
        yt-dlp)
            result_path="$job_out_dir/result.json"
            local url
            url=$(jq -r '.payload.url // empty' "$json_file")
            if [[ -z "$url" ]]; then
                exec_status=1
                error_msg="yt-dlp: missing payload.url"
            else
                if command -v yt-dlp &>/dev/null; then
                    yt-dlp --dump-json "$url" > "$result_path" || exec_status=$?
                else
                    jq -n --arg u "$url" '{"warning":"yt-dlp CLI not found, simulated","url":$u}' > "$result_path"
                fi
            fi
            ;;
        fabric)
            result_path="$job_out_dir/result.json"
            local pattern
            pattern=$(jq -r '.payload.pattern // "summarize"' "$json_file")
            local text
            text=$(jq -r '.payload.text // "empty text"' "$json_file")
            if command -v fabric &>/dev/null; then
                echo "$text" | fabric --pattern "$pattern" > "$job_out_dir/output.md" || exec_status=$?
                jq -n --arg p "$pattern" --arg path "$job_out_dir/output.md" '{"pattern":$p,"output_path":$path}' > "$result_path"
            else
                jq -n --arg p "$pattern" --arg t "$text" '{"warning":"fabric CLI not found, simulated","pattern":$p,"text":$t}' > "$result_path"
            fi
            ;;
        *)
            exec_status=1
            error_msg="Unknown tool_name: $tool_name"
            ;;
    esac

    if [[ $exec_status -eq 0 ]]; then
        local body
        body=$(jq -n --arg path "/spool/out/$job_id/result.json" '{"status":"completed","result_path":$path}')
        send_status "$job_id" "completed" "$body"
        echo "Job $job_id completed successfully."
    else
        [[ -z "$error_msg" ]] && error_msg="Tool $tool_name exited with code $exec_status"
        local body
        body=$(jq -n --arg err "$error_msg" '{"status":"failed","error_msg":$err}')
        send_status "$job_id" "failed" "$body"
        echo "Job $job_id failed: $error_msg" >&2
    fi

    rm -f "$json_file"
}

run_once() {
    local file
    for file in "$IN_DIR"/*.json; do
        [[ -e "$file" ]] || continue
        local filename
        filename=$(basename "$file")
        local processing_file="$PROCESSING_DIR/$filename"

        if mv "$file" "$processing_file" 2>/dev/null; then
            process_job "$processing_file"
        fi
    done
}

if [[ "${1:-}" == "--loop" ]]; then
    echo "Starting spool-runner in daemon mode..."
    while true; do
        run_once
        sleep 2
    done
else
    run_once
fi
