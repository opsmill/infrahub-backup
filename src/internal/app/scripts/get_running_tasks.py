import json

from infrahub_sdk import InfrahubClientSync
from infrahub_sdk.config import Config

from infrahub_sdk.task.models import TaskFilter, TaskState

client = InfrahubClientSync(config=Config(pagination_size=200))
tasks = client.task.filter(
    filter=TaskFilter(state=[TaskState.PENDING, TaskState.RUNNING])
)

print(json.dumps([json.loads(task.model_dump_json()) for task in tasks]))
