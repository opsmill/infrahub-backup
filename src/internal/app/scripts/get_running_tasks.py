import json

from infrahub_sdk import InfrahubClientSync
from infrahub_sdk.task.models import TaskFilter, TaskState

# pagination_size is supplied via the INFRAHUB_PAGINATION_SIZE environment
# variable, which the backup tool clamps to the task-manager (Prefect) cap.
client = InfrahubClientSync()
tasks = client.task.filter(filter=TaskFilter(state=[TaskState.PENDING, TaskState.RUNNING]))

print(json.dumps([json.loads(task.model_dump_json()) for task in tasks]))
