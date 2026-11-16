# Test data of the traces collector

- `search_response.json` and `trace_raw.json` are answers of a local
  `grafana/tempo:2.10.3`, recorded on 2026-11-05. Tempo ran with the Infrahub
  development config (`development/tempo/config.yml` of opsmill/infrahub) plus
  `endpoint: 0.0.0.0:4318` under the OTLP HTTP receiver. The spans were sent
  with the OpenTelemetry Python SDK and hold fake personal data on purpose.
  - `search_response.json`: `GET /api/search?q={ traceDuration > 1s }&limit=10000`.
    Tempo returned the three traces out of duration order, and dropped the
    leading zero of one trace ID (31 hex characters).
  - `trace_raw.json`: `GET /api/v2/traces/<id>` with `Accept: application/json`
    for the `POST /graphql` trace.
- `sample_ratio_enabled.json` and `sample_ratio_disabled.json` are written by
  hand from the `TraceSampleRatioAPI` model of
  `backend/infrahub/api/troubleshooting.py` (opsmill/infrahub), not recorded.
