# A generic HTTP load: each user asks for / every 1 to 2 seconds. The target is the container
# instance's nginx (LOCUST_HOST, which the task definition sets).
from locust import HttpUser, between, task


class Visitor(HttpUser):
    wait_time = between(1, 2)

    @task
    def index(self):
        self.client.get("/")
