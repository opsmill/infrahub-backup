"""Crash flow runs stuck in RUNNING or PENDING beyond the retention window.

The `infrahub tasks flush stale-runs` CLI command cannot be used for this: it calls
`PrefectTask.delete_flow_runs` with a hardcoded `states=[RUNNING]`, so a run left in
PENDING — a worker that died between accepting a run and starting it — is never
cleared. This script drives that same internal helper instead, with PENDING added to
the state list, and falls back to a standalone Prefect implementation on deployments
where the Infrahub internals are not importable.

Selecting by `start_time` covers PENDING runs even though Prefect only stamps
start_time on the first RUNNING transition: server-side, the filter compares
`coalesce(start_time, expected_start_time)`, and every run carries an
expected_start_time from its first state change.
"""

import asyncio
import os
import sys
from datetime import datetime, timedelta, timezone

from prefect import State
from prefect.client.orchestration import get_client
from prefect.client.schemas.filters import (
    FlowRunFilter,
    FlowRunFilterStartTime,
    FlowRunFilterState,
    FlowRunFilterStateType,
)
from prefect.client.schemas.objects import StateType
from prefect.logging.loggers import get_logger

STALE_STATES = [StateType.RUNNING, StateType.PENDING]


class InfrahubInternalsUnavailable(Exception):
    """Infrahub's own flow-run cleanup helper is missing or takes different arguments."""


async def crash_stale_flow_runs_with_infrahub(days_to_keep: int, batch_size: int) -> None:
    """Crash stale flow runs through Infrahub's own cleanup helper."""
    try:
        from infrahub import config
        from infrahub.task_manager.task import PrefectTask
    except Exception as exc:
        raise InfrahubInternalsUnavailable(f"Infrahub internals are not importable ({exc})") from exc

    if not hasattr(PrefectTask, "delete_flow_runs"):
        raise InfrahubInternalsUnavailable("PrefectTask has no delete_flow_runs helper")

    # Mirrors what the CLI command does before calling the helper. A missing config file
    # is fine, settings then come from the environment; an invalid one is not worth
    # aborting over, since the cleanup itself only needs the task-manager address.
    try:
        config.load_and_exit(config_file_name=os.environ.get("INFRAHUB_CONFIG", "infrahub.toml"))
    except SystemExit as exc:
        raise InfrahubInternalsUnavailable("Infrahub configuration is not valid") from exc
    except Exception as exc:
        raise InfrahubInternalsUnavailable(f"Infrahub configuration could not be loaded ({exc})") from exc

    try:
        await PrefectTask.delete_flow_runs(
            states=STALE_STATES,
            delete=False,
            days_to_keep=days_to_keep,
            batch_size=batch_size,
        )
    except TypeError as exc:
        raise InfrahubInternalsUnavailable(f"delete_flow_runs does not accept the expected arguments ({exc})") from exc


async def crash_stale_flow_runs(days_to_keep: int, batch_size: int) -> None:
    """Crash stale flow runs directly through the Prefect API."""
    logger = get_logger()

    async with get_client() as client:
        cutoff = datetime.now(timezone.utc) - timedelta(days=days_to_keep)

        # Create filter for old runs still in a non-terminal state
        # Note: Using start_time because created time filtering is not available
        flow_run_filter = FlowRunFilter(
            start_time=FlowRunFilterStartTime(before_=cutoff),
            state=FlowRunFilterState(type=FlowRunFilterStateType(any_=STALE_STATES)),
        )

        # Get flow runs to crash
        flow_runs = await client.read_flow_runs(flow_run_filter=flow_run_filter, limit=batch_size)

        crashed_total = 0

        while flow_runs:
            batch_crashed = 0
            failed_crashes = []

            # Crash each flow run through the API
            for flow_run in flow_runs:
                try:
                    await client.set_flow_run_state(
                        flow_run_id=flow_run.id,
                        state=State(type=StateType.CRASHED),
                        force=True,
                    )
                    crashed_total += 1
                    batch_crashed += 1
                except Exception as e:
                    logger.warning(f"Failed to set flow run {flow_run.id} to CRASHED: {e}")
                    failed_crashes.append(flow_run.id)

                # Rate limiting - adjust based on your API capacity
                if batch_crashed % 10 == 0:
                    await asyncio.sleep(0.5)

            logger.info(f"Set {batch_crashed}/{len(flow_runs)} flow runs to CRASHED (total: {crashed_total})")
            if failed_crashes:
                logger.warning(f"Failed to set {len(failed_crashes)} CRASHED flow runs")

            # Get next batch
            previous_flow_run_ids = [flow_run.id for flow_run in flow_runs]
            flow_runs = await client.read_flow_runs(flow_run_filter=flow_run_filter, limit=batch_size)

            # Every run in the batch failed to transition, so the same batch comes back
            # on every read: stop instead of looping on it forever.
            if [flow_run.id for flow_run in flow_runs] == previous_flow_run_ids:
                logger.info("Found same flow runs to crash, aborting")
                break

            # Delay between batches to avoid overwhelming the API
            await asyncio.sleep(1.0)

        logger.info(f"Retention complete. Total CRASHED: {crashed_total}")


async def main(days_to_keep: int, batch_size: int) -> None:
    try:
        await crash_stale_flow_runs_with_infrahub(days_to_keep=days_to_keep, batch_size=batch_size)
    except InfrahubInternalsUnavailable as exc:
        get_logger().info(f"Using the standalone stale-run cleanup: {exc}")
        await crash_stale_flow_runs(days_to_keep=days_to_keep, batch_size=batch_size)


asyncio.run(main(days_to_keep=int(sys.argv[1]), batch_size=int(sys.argv[2])))
