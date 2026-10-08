"""Run development database contracts without silently skipping a database."""

import json
import os
from pathlib import Path
import subprocess
import sys


def validate_environment(environment):
    required = [
        "TEST_MYSQL_DSN", "TEST_MYSQL_LOG_DSN",
        "TEST_POSTGRES_DSN", "TEST_POSTGRES_LOG_DSN",
        "TEST_WS_MANAGER_REDIS_ADDR",
    ]
    missing = [name for name in required if not environment.get(name, "").strip()]
    if missing:
        raise ValueError("missing development test configuration: " + ", ".join(missing))


def validate_results(events):
    completed = set()
    for event in events:
        identity = (event.get("Package", ""), event.get("Test", ""))
        if event.get("Action") == "skip":
            raise ValueError("database matrix skipped: " + "/".join(identity))
        if event.get("Action") == "fail":
            raise ValueError("database matrix failed: " + "/".join(identity))
        if event.get("Action") == "pass":
            completed.add(identity)
    required = set()
    for package, test in [
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/account_retry_uses_one_bill_and_observed_boundaries/known_429"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/account_retry_uses_one_bill_and_observed_boundaries/unknown_submission"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/account_retry_uses_one_bill_and_observed_boundaries/output_started"),
        ("controller", "TestAccountAffinityDatabaseMatrix/{dialect}/conditional_delete_keeps_newly_enabled_channel/by_status"),
        ("controller", "TestAccountAffinityDatabaseMatrix/{dialect}/conditional_delete_keeps_newly_enabled_channel/disabled"),
        ("controller", "TestAccountAffinityDatabaseMatrix/{dialect}/strict_pins_actual_account"),
        ("controller", "TestAccountAffinityDatabaseMatrix/{dialect}/concurrent_strict_claim_and_expiry"),
        ("controller", "TestAccountAffinityDatabaseMatrix/{dialect}/off_balances_and_prefer_falls_back"),
        ("controller", "TestAccountAffinityDatabaseMatrix/{dialect}/rotation_reorder_and_retirement"),
        ("controller", "TestAccountAffinityDatabaseMatrix/{dialect}/account_health_survives_key_reordering"),
        ("controller", "TestAccountAffinityDatabaseMatrix/{dialect}/task_preserves_submitting_account"),
        ("controller", "TestAccountAffinityDatabaseMatrix/{dialect}/channel_delete_retires_identity"),
        ("controller", "TestAccountAffinityDatabaseMatrix/{dialect}/credential_api_permission_and_redaction"),
        ("controller", "TestAccountAffinityDatabaseMatrix/{dialect}/same_session_isolated_by_user"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/image_stream_budget_preserves_native_quantity/count_stop"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/image_stream_budget_preserves_native_quantity/count_legacy"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/image_stream_budget_preserves_native_quantity/count_healthy"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/image_stream_budget_preserves_native_quantity/token_stop"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/image_stream_budget_preserves_native_quantity/token_healthy"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/image_stream_budget_preserves_native_quantity/storage"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/image_stream_budget_preserves_native_quantity/json_stop"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/phase_possible"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/phase_accepted"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/relay_boundary_evidence_failures_preserve_funds/success"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/relay_boundary_evidence_failures_preserve_funds/zero_write"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/relay_boundary_evidence_failures_preserve_funds/partial_write"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/relay_boundary_evidence_failures_preserve_funds/possible_storage"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/relay_boundary_evidence_failures_preserve_funds/accepted_storage"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/relay_boundary_evidence_failures_preserve_funds/response_storage"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/realtime_budget_stops_before_output_without_debt/evidence_failure"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/buffered_responses_budget_preserves_json_and_accounting/insufficient"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/buffered_responses_budget_preserves_json_and_accounting/storage_failure"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/buffered_responses_budget_preserves_json_and_accounting/healthy"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/buffered_responses_budget_preserves_json_and_accounting/fixed"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/realtime_budget_stops_before_output_without_debt/estimated_text"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/realtime_budget_stops_before_output_without_debt/estimated_audio"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/realtime_budget_stops_before_output_without_debt/storage_failure"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/realtime_budget_stops_before_output_without_debt/healthy"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/realtime_budget_stops_before_output_without_debt/prior_receipt"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/realtime_budget_stops_before_output_without_debt/rejected_client_input"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/realtime_budget_stops_before_output_without_debt/accepted_client_input"),
        ("model", "TestMigrationSchemaStability"),
        ("model", "TestRequestPolicyDatabaseMatrix"),
        ("model", "TestCreditPackDatabaseMatrix"),
        ("model", "TestCreditPackDatabaseMatrix/{dialect}/usage_evidence_survives_pre_intent_exit"),
        ("model", "TestCreditPackDatabaseMatrix/{dialect}/usage_sequence_conflict_is_not_a_second_receipt"),
        ("model", "TestCreditPackDatabaseMatrix/{dialect}/bill_adjustment_preserves_original_fefo_and_never_collects_again"),
        ("model", "TestCreditPackDatabaseMatrix/{dialect}/bill_adjustment_uses_original_window_generations"),
        ("model", "TestCreditPackDatabaseMatrix/{dialect}/settlement_outbox_and_log_response_loss"),
        ("model", "TestCreditPackDatabaseMatrix/{dialect}/settlement_survives_soft_deleted_key"),
        ("model", "TestCreditPackDatabaseMatrix/{dialect}/unknown_bill_review_preserves_original_evidence"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/credit_admin_API_contract/owned_bill_and_controlled_adjustment_API"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/credit_admin_API_contract/unknown_bill_controlled_review_API"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/pending_retry_recovery"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/pending_retry_free_recovery"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/pending"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/reported_zero_and_missing_usage_are_distinct"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/estimator_provenance_uses_count_time_model_and_settings/enabled_true"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/estimator_provenance_uses_count_time_model_and_settings/enabled_false"),
        ("model", "TestCreditPackDatabaseMatrix/{dialect}/estimator_metadata_is_bounded_and_keeps_original_counter"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/responses_websocket_budget_stops_before_output_without_debt/insufficient"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/responses_websocket_budget_stops_before_output_without_debt/storage_failure"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/responses_websocket_budget_stops_before_output_without_debt/healthy"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/responses_websocket_budget_stops_before_output_without_debt/fixed"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/retry_records_effective_price_without_overwriting_admission"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/chat_responses_estimated"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/chat_responses_healthy"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/chat_responses_storage"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/responses_chat_estimated"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/responses_chat_healthy"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/responses_chat_storage"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/reported_zero_and_missing_usage_are_distinct/rcreported"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/reported_zero_and_missing_usage_are_distinct/rcmissing"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/reported_zero_and_missing_usage_are_distinct/rccacheunknown"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/reported_zero_and_missing_usage_are_distinct/rccachezero"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/speech_legacy"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/speech_estimated"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/speech_storage"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/speech_healthy"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/transcription_estimated"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/transcription_storage"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/transcription_healthy"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/responses_estimated"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/responses_storage_failure"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/responses_healthy"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/responses_request_price"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/claude_chat_estimated"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/claude_native_estimated"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/claude_responses_estimated"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/gemini_chat_estimated"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/gemini_native_estimated"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/gemini_responses_estimated"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/reported"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/estimated"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/storage_failure"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/request_price"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/stream_budget_stops_output_without_user_debt/healthy_tokens"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/audio_pricing_is_frozen_before_upstream"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/reported_zero_and_missing_usage_are_distinct/geminireported"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/reported_zero_and_missing_usage_are_distinct/claudecacheunknown"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/responses_websocket_every_create_has_its_own_submitted_bill"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/image_growth_and_audio_settle_source_packs/transcription"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/image_growth_and_audio_settle_source_packs/transcriptionstream"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/tool_prices_are_frozen_before_upstream/initial_10"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/tool_prices_are_frozen_before_upstream/initial_0"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/image_growth_and_audio_settle_source_packs/image"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/image_growth_and_audio_settle_source_packs/speechstream"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/image_growth_and_audio_settle_source_packs/speechheaders"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/image_growth_and_audio_settle_source_packs/speechheaderszero"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/pending_over_budget"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/pending_recovery"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/immediate"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/fractional_credit"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/pending_polled"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/pending_native_integer"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/pending_polled_recovery"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/immediate_over_budget"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/immediate_clamped"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/zero"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/zero_unknown"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/constant_free"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/selective_free"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/task_plugin_keeps_holds_until_terminal_and_replays_once/pending_zero_unknown"),
        ("controller", "TestCreditBillingDatabaseMatrix/{dialect}/midjourney_holds_then_settles_without_legacy_balance"),
        ("model", "TestSubscriptionVersionDatabaseMatrix"),
        ("model", "TestSubscriptionVersionDatabaseMatrix/{dialect}/window_consumption"),
        ("service", "TestFixedPriceBillingDatabaseMatrix"),
        ("controller", "TestPreConsumePolicyDatabaseMatrix"),
        ("controller", "TestRequestPolicyRoutingDatabaseMatrix"),
        ("controller", "TestDeleteRedemptionBatch"),
        ("controller", "TestCreditBillingDatabaseMatrix"),
    ]:
        for dialect in ["sqlite", "mysql", "postgres"]:
            required.add((f"github.com/QuantumNous/new-api/{package}", test.format(dialect=dialect) if "{dialect}" in test else f"{test}/{dialect}"))
    required.add(("github.com/QuantumNous/new-api/pkg/wsmanager", "TestRedisChannelCloseEventsStayWithinDatabase"))
    missing = required - completed
    if missing:
        raise ValueError("database matrix missing completed contracts: " + ", ".join("/".join(item) for item in sorted(missing)))


def main():
    environment = dict(os.environ)
    try:
        validate_environment(environment)
    except ValueError as error:
        print(error, file=sys.stderr)
        return 1
    environment["GOWORK"] = "off"
    for dialect in ["MYSQL", "POSTGRES"]:
        environment.setdefault(f"AUDIT_{dialect}_DSN", environment[f"TEST_{dialect}_DSN"])
        for suffix in ["DSN", "LOG_DSN"]:
            environment.setdefault(f"TEST_FIXED_{dialect}_{suffix}", environment[f"TEST_{dialect}_{suffix}"])
    command = [
        "go", "test", "-json", "-count=1", "-p", "1", "-timeout", "10m",
        "./model", "./service", "./controller", "./pkg/wsmanager",
        "-run", "^Test(.*DatabaseMatrix|MigrationSchemaStability|DeleteRedemptionBatch|RedisChannelCloseEventsStayWithinDatabase)$",
    ]
    events = []
    with subprocess.Popen(command, cwd=Path(__file__).resolve().parent.parent,
                          env=environment, stdout=subprocess.PIPE, text=True) as process:
        for line in process.stdout:
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                print(line, end="")
                continue
            events.append(event)
            if "Output" in event:
                print(event["Output"], end="", flush=True)
        exit_code = process.wait()
    if exit_code:
        return exit_code
    try:
        validate_results(events)
    except ValueError as error:
        print(error, file=sys.stderr)
        return 1
    print("Development database matrix passed: SQLite, MySQL, PostgreSQL and real Redis; no skipped contracts.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
