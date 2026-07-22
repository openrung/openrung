import io
import json
import sys
import types
import unittest
from unittest import mock

try:
    import boto3  # noqa: F401
except ModuleNotFoundError:
    boto3_stub = types.ModuleType("boto3")
    boto3_stub.client = mock.Mock()
    boto3_stub.resource = mock.Mock()
    botocore_stub = types.ModuleType("botocore")
    exceptions_stub = types.ModuleType("botocore.exceptions")

    class StubClientError(Exception):
        def __init__(self, response=None, operation_name=""):
            super().__init__(operation_name)
            self.response = response or {}

    exceptions_stub.ClientError = StubClientError
    sys.modules["boto3"] = boto3_stub
    sys.modules["botocore"] = botocore_stub
    sys.modules["botocore.exceptions"] = exceptions_stub

import lambda_function as sync


class Response(io.BytesIO):
    status = 200

    def __init__(self, value):
        super().__init__(value)
        self.headers = {}

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        self.close()


class FakeLightsail:
    def __init__(self, states):
        self.states = states
        self.writes = []

    def get_instance_port_states(self, instanceName):
        return {"portStates": self.states}

    def put_instance_public_ports(self, instanceName, portInfos):
        self.writes.append(portInfos)
        self.states = [dict(item, state="open") for item in portInfos]


class FirewallSyncTest(unittest.TestCase):
    def test_targets_are_strict_and_unique(self):
        self.assertEqual(sync._targets('[{"region":"eu-central-1","instance":"relay-a"}]')[0]["instance"], "relay-a")
        for raw in ("[]", '[{"region":"bad","instance":"relay-a"}]', '[{"region":"eu-central-1","instance":"relay-a","extra":1}]'):
            with self.assertRaises(ValueError):
                sync._targets(raw)

    def test_feed_is_canonical_ipv4_origin_only(self):
        feed = {"syncToken": "7", "prefixes": [
            {"ip_prefix": "203.0.113.0/24", "service": sync.ORIGIN_SERVICE},
            {"ip_prefix": "198.51.100.0/24", "service": "CLOUDFRONT"},
        ]}
        result, token = sync._download_prefixes(lambda *_args, **_kwargs: Response(json.dumps(feed).encode()))
        self.assertEqual(result, ["203.0.113.0/24"])
        self.assertEqual(token, 7)

    def test_sync_adds_before_removing(self):
        client = FakeLightsail([
            {"fromPort": 443, "toPort": 443, "protocol": "tcp", "state": "open", "cidrs": ["0.0.0.0/0"]},
            {"fromPort": 8443, "toPort": 8443, "protocol": "tcp", "state": "open", "cidrs": ["192.0.2.0/24"]},
        ])
        self.assertTrue(sync._sync_target(client, "relay-a", ["203.0.113.0/24"]))
        self.assertEqual(len(client.writes), 2)
        self.assertEqual(sync._origin_prefixes(client.writes[0]), ["192.0.2.0/24", "203.0.113.0/24"])
        self.assertEqual(sync._origin_prefixes(client.writes[1]), ["203.0.113.0/24"])
        self.assertEqual(client.writes[1][0]["cidrs"], ["0.0.0.0/0"])

    def test_world_open_origin_is_rejected_without_write(self):
        client = FakeLightsail([
            {"fromPort": 8443, "toPort": 8443, "protocol": "tcp", "state": "open", "cidrs": ["0.0.0.0/0"]},
        ])
        with self.assertRaises(ValueError):
            sync._sync_target(client, "relay-a", ["203.0.113.0/24"])
        self.assertEqual(client.writes, [])

    def test_unchanged_network_order_is_a_noop(self):
        client = FakeLightsail([
            {"fromPort": 8443, "toPort": 8443, "protocol": "tcp", "state": "open", "cidrs": [
                "13.124.199.0/24", "3.172.0.0/18", "130.176.0.0/18"
            ]},
        ])
        desired = ["3.172.0.0/18", "13.124.199.0/24", "130.176.0.0/18"]
        self.assertFalse(sync._sync_target(client, "relay-a", desired))
        self.assertEqual(client.writes, [])

    def test_notification_url_is_exact(self):
        event = {"Records": [{"EventSource": "aws:sns", "Sns": {"Message": json.dumps({
            "url": sync.AWS_IP_RANGES_URL, "syncToken": "9"
        })}}]}
        self.assertEqual(sync._notification_sync_token(event), 9)
        event["Records"][0]["Sns"]["Message"] = json.dumps({"url": "https://example.com/feed", "syncToken": "9"})
        with self.assertRaises(ValueError):
            sync._notification_sync_token(event)


if __name__ == "__main__":
    unittest.main()
