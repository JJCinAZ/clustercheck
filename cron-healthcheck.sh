#!/usr/bin/env bash

set -u

healthcheck_url="${HEALTHCHECK_URL:-https://hc-ping.com/838ffa36-2e24-430b-a0cd-5c66138e265e}"
clustercheck_url="${CLUSTERCHECK_URL:-http://127.0.0.1:8080/cluster}"
curl_bin="${CURL_BIN:-curl}"
curl_timeout="${CURL_TIMEOUT:-10}"

# curl must be called with the --fail so that it will return exit code 22 for HTTP Error ≥ 400
if "${curl_bin}" --fail --silent --show-error --max-time "${curl_timeout}" "${clustercheck_url}" >/dev/null; then
	"${curl_bin}" --fail --silent --show-error --max-time "${curl_timeout}" "${healthcheck_url}" >/dev/null
	exit $?
fi

"${curl_bin}" --fail --silent --show-error --max-time "${curl_timeout}" "${healthcheck_url}/fail" >/dev/null
exit 1
