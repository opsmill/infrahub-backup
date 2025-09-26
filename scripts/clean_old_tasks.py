import asyncio
import sys

from datetime import datetime, timedelta, timezone
from prefect.logging.loggers import get_logger
from prefect import flow, task, get_run_logger
from prefect.client.orchestration import get_client
from prefect.client.schemas.filters import (
    FlowRunFilter,
    FlowRunFilterState,
    FlowRunFilterStateType,
    FlowRunFilterStartTime,
)
from prefect.client.schemas.objects import StateType


async def delete_old_flow_runs(days_to_keep: int = 30, batch_size: int = 100):
    """Delete completed flow runs older than specified days."""
    logger = get_logger()

    async with get_client() as client:
        cutoff = datetime.now(timezone.utc) - timedelta(days=days_to_keep)

        # Create filter for old completed flow runs
        # Note: Using start_time because created time filtering is not available
        flow_run_filter = FlowRunFilter(
            start_time=FlowRunFilterStartTime(before_=cutoff),
            state=FlowRunFilterState(
                type=FlowRunFilterStateType(
                    any_=[StateType.COMPLETED, StateType.FAILED, StateType.CANCELLED]
                )
            ),
        )

        # Get flow runs to delete
        flow_runs = await client.read_flow_runs(
            flow_run_filter=flow_run_filter, limit=batch_size
        )

        deleted_total = 0

        while flow_runs:
            batch_deleted = 0
            failed_deletes = []

            # Delete each flow run through the API
            for flow_run in flow_runs:
                try:
                    await client.delete_flow_run(flow_run.id)
                    deleted_total += 1
                    batch_deleted += 1
                except Exception as e:
                    logger.warning(f"Failed to delete flow run {flow_run.id}: {e}")
                    failed_deletes.append(flow_run.id)

                # Rate limiting - adjust based on your API capacity
                if batch_deleted % 10 == 0:
                    await asyncio.sleep(0.5)

            logger.info(
                f"Deleted {batch_deleted}/{len(flow_runs)} flow runs (total: {deleted_total})"
            )
            if failed_deletes:
                logger.warning(f"Failed to delete {len(failed_deletes)} flow runs")

            # Get next batch
            flow_runs = await client.read_flow_runs(
                flow_run_filter=flow_run_filter, limit=batch_size
            )

            # Delay between batches to avoid overwhelming the API
            await asyncio.sleep(1.0)

        logger.info(f"Retention complete. Total deleted: {deleted_total}")


asyncio.run(
    delete_old_flow_runs(days_to_keep=int(sys.argv[1]), batch_size=int(sys.argv[2]))
)
