"""Keep relay-local Lightsail WSS origins restricted to CloudFront.

The function intentionally logs only aggregate, label-free counters.  Target
names and regions are configuration, never log fields.  A single invocation
updates every configured relay with add-before-remove semantics and preserves
the last known-good firewall on feed or validation failure.
"""

from __future__ import annotations

import ipaddress
import json
import logging
import os
import re
import time
import urllib.request
import uuid
from typing import Any, Callable

import boto3
from botocore.exceptions import ClientError


AWS_IP_RANGES_URL = "https://ip-ranges.amazonaws.com/ip-ranges.json"
ORIGIN_SERVICE = "CLOUDFRONT_ORIGIN_FACING"
LOCK_NAME = "cloudfront-origin-firewall-sync"
MAX_FEED_BYTES = 5 << 20
MAX_PREFIXES = 60
MAX_TARGETS = 100
# The provider calls span every configured region sequentially. Keep the lease
# longer than the Lambda timeout so a timed-out invocation remains fail-closed
# and cannot overlap its successor while AWS finishes tearing it down.
LOCK_SECONDS = 180
NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$")
REGION_RE = re.compile(r"^[a-z]{2}(?:-gov)?-[a-z]+-\d$")

logger = logging.getLogger()
logger.setLevel(logging.INFO)


def _targets(raw: str) -> list[dict[str, str]]:
    try:
        value = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ValueError("TARGETS_JSON must be valid JSON") from exc
    if not isinstance(value, list) or not value or len(value) > MAX_TARGETS:
        raise ValueError(f"TARGETS_JSON must contain 1..{MAX_TARGETS} targets")
    result: list[dict[str, str]] = []
    seen: set[tuple[str, str]] = set()
    for item in value:
        if not isinstance(item, dict) or set(item) != {"region", "instance"}:
            raise ValueError("each target must contain only region and instance")
        region, instance = item.get("region"), item.get("instance")
        if not isinstance(region, str) or not REGION_RE.fullmatch(region):
            raise ValueError("target region is invalid")
        if not isinstance(instance, str) or not NAME_RE.fullmatch(instance):
            raise ValueError("target instance is invalid")
        key = (region, instance)
        if key in seen:
            raise ValueError("TARGETS_JSON contains a duplicate target")
        seen.add(key)
        result.append({"region": region, "instance": instance})
    return result


def _notification_sync_token(event: Any) -> int | None:
    """Validate an AWS IP-space SNS event without trusting a supplied URL."""
    if not isinstance(event, dict) or "Records" not in event:
        return None
    records = event.get("Records")
    if not isinstance(records, list) or len(records) != 1:
        raise ValueError("SNS invocation must contain exactly one record")
    record = records[0]
    if not isinstance(record, dict) or record.get("EventSource") != "aws:sns":
        raise ValueError("unexpected event source")
    message = record.get("Sns", {}).get("Message")
    if not isinstance(message, str):
        raise ValueError("SNS message is missing")
    try:
        payload = json.loads(message)
    except json.JSONDecodeError as exc:
        raise ValueError("SNS message is not valid JSON") from exc
    if payload.get("url") != AWS_IP_RANGES_URL:
        raise ValueError("SNS message contains an unexpected feed URL")
    token = payload.get("syncToken")
    if isinstance(token, str) and token.isdigit():
        return int(token)
    if isinstance(token, int) and token >= 0:
        return token
    raise ValueError("SNS message contains an invalid sync token")


def _prefix_sort_key(value: str) -> tuple[int, int]:
    network = ipaddress.ip_network(value)
    return int(network.network_address), network.prefixlen


def _download_prefixes(
    opener: Callable[..., Any] = urllib.request.urlopen,
) -> tuple[list[str], int]:
    request = urllib.request.Request(
        AWS_IP_RANGES_URL,
        headers={"User-Agent": "openrung-cloudfront-origin-sync/1"},
    )
    with opener(request, timeout=10) as response:
        status = getattr(response, "status", 200)
        if status != 200:
            raise ValueError("AWS IP range feed returned a non-200 response")
        length = response.headers.get("Content-Length")
        if length is not None and int(length) > MAX_FEED_BYTES:
            raise ValueError("AWS IP range feed exceeds the size bound")
        raw = response.read(MAX_FEED_BYTES + 1)
    if len(raw) > MAX_FEED_BYTES:
        raise ValueError("AWS IP range feed exceeds the size bound")
    try:
        feed = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ValueError("AWS IP range feed is not valid JSON") from exc
    if not isinstance(feed, dict) or not isinstance(feed.get("prefixes"), list):
        raise ValueError("AWS IP range feed has an invalid schema")
    token = feed.get("syncToken")
    if not isinstance(token, str) or not token.isdigit():
        raise ValueError("AWS IP range feed has an invalid sync token")
    prefixes: set[str] = set()
    for item in feed["prefixes"]:
        if not isinstance(item, dict):
            raise ValueError("AWS IP range feed contains an invalid prefix item")
        if item.get("service") != ORIGIN_SERVICE:
            continue
        raw_prefix = item.get("ip_prefix")
        if not isinstance(raw_prefix, str):
            raise ValueError("origin-facing range has no IPv4 prefix")
        try:
            network = ipaddress.ip_network(raw_prefix, strict=True)
        except ValueError as exc:
            raise ValueError("origin-facing range is not canonical") from exc
        if network.version != 4 or str(network) != raw_prefix:
            raise ValueError("origin-facing range is not canonical IPv4")
        if network.prefixlen == 0:
            raise ValueError("origin-facing range must not be world-open")
        prefixes.add(raw_prefix)
    result = sorted(prefixes, key=_prefix_sort_key)
    if not result or len(result) > MAX_PREFIXES:
        raise ValueError(f"origin-facing IPv4 range count must be 1..{MAX_PREFIXES}")
    return result, int(token)


def _port_info(state: dict[str, Any]) -> dict[str, Any]:
    info: dict[str, Any] = {
        "fromPort": state["fromPort"],
        "toPort": state["toPort"],
        "protocol": state["protocol"],
    }
    for key in ("cidrs", "ipv6Cidrs", "cidrListAliases"):
        values = state.get(key)
        if values:
            info[key] = sorted(set(values))
    return info


def _rule(prefixes: list[str]) -> dict[str, Any]:
    return {"fromPort": 8443, "toPort": 8443, "protocol": "tcp", "cidrs": prefixes}


def _sort_prefixes(prefixes: Any) -> list[str]:
    return sorted(set(prefixes), key=_prefix_sort_key)


def _replace_origin_rule(port_infos: list[dict[str, Any]], replacement: dict[str, Any]) -> list[dict[str, Any]]:
    result: list[dict[str, Any]] = []
    replaced = False
    for info in port_infos:
        if info["fromPort"] == 8443 or info["toPort"] == 8443:
            if info["fromPort"] != 8443 or info["toPort"] != 8443 or info["protocol"] != "tcp" or replaced:
                raise ValueError("port 8443 has an ambiguous firewall configuration")
            if info.get("ipv6Cidrs") or info.get("cidrListAliases"):
                raise ValueError("port 8443 must be IPv4 CIDR-only")
            for prefix in info.get("cidrs", []):
                network = ipaddress.ip_network(prefix, strict=True)
                if network.version != 4 or network.prefixlen == 0:
                    raise ValueError("port 8443 contains an unsafe CIDR")
            result.append(replacement)
            replaced = True
        else:
            result.append(info)
    if not replaced:
        result.append(replacement)
    return sorted(result, key=lambda item: (item["fromPort"], item["toPort"], item["protocol"]))


def _current(lightsail: Any, instance: str) -> list[dict[str, Any]]:
    response = lightsail.get_instance_port_states(instanceName=instance)
    states = response.get("portStates")
    if not isinstance(states, list):
        raise ValueError("Lightsail returned invalid port state")
    return [_port_info(item) for item in states if item.get("state", "open") == "open"]


def _origin_prefixes(port_infos: list[dict[str, Any]]) -> list[str] | None:
    matches = [item for item in port_infos if item["fromPort"] <= 8443 <= item["toPort"]]
    if not matches:
        return None
    if len(matches) != 1 or matches[0]["fromPort"] != 8443 or matches[0]["toPort"] != 8443 or matches[0]["protocol"] != "tcp":
        raise ValueError("port 8443 has an ambiguous firewall configuration")
    item = matches[0]
    if item.get("ipv6Cidrs") or item.get("cidrListAliases"):
        raise ValueError("port 8443 must be IPv4 CIDR-only")
    prefixes = item.get("cidrs", [])
    for prefix in prefixes:
        network = ipaddress.ip_network(prefix, strict=True)
        if network.version != 4 or network.prefixlen == 0:
            raise ValueError("port 8443 contains an unsafe CIDR")
    return _sort_prefixes(prefixes)


def _put_and_verify(lightsail: Any, instance: str, infos: list[dict[str, Any]], expected: list[str]) -> None:
    lightsail.put_instance_public_ports(instanceName=instance, portInfos=infos)
    observed = _origin_prefixes(_current(lightsail, instance))
    if observed != _sort_prefixes(expected):
        raise RuntimeError("Lightsail did not converge to the requested port 8443 rule")


def _sync_target(lightsail: Any, instance: str, desired: list[str]) -> bool:
    current_infos = _current(lightsail, instance)
    current_prefixes = _origin_prefixes(current_infos)
    if current_prefixes == desired:
        return False
    union = _sort_prefixes(set(current_prefixes or []).union(desired))
    if len(union) > MAX_PREFIXES:
        raise ValueError("add-before-remove union exceeds the provider source-address quota")
    union_infos = _replace_origin_rule(current_infos, _rule(union))
    _put_and_verify(lightsail, instance, union_infos, union)
    if union != desired:
        final_infos = _replace_origin_rule(union_infos, _rule(desired))
        _put_and_verify(lightsail, instance, final_infos, desired)
    return True


def _acquire_lock(table: Any, owner: str, now: int) -> bool:
    try:
        table.put_item(
            Item={"LockName": LOCK_NAME, "LeaseOwner": owner, "ExpiresAt": now + LOCK_SECONDS},
            ConditionExpression="attribute_not_exists(LockName) OR ExpiresAt < :now",
            ExpressionAttributeValues={":now": now},
        )
        return True
    except ClientError as exc:
        if exc.response.get("Error", {}).get("Code") == "ConditionalCheckFailedException":
            return False
        raise


def _release_lock(table: Any, owner: str) -> None:
    table.delete_item(
        Key={"LockName": LOCK_NAME},
        ConditionExpression="#lease_owner = :owner",
        ExpressionAttributeNames={"#lease_owner": "LeaseOwner"},
        ExpressionAttributeValues={":owner": owner},
    )


def lambda_handler(event: Any, context: Any) -> dict[str, Any]:
    targets = _targets(os.environ.get("TARGETS_JSON", ""))
    notification_token = _notification_sync_token(event)
    prefixes, sync_token = _download_prefixes()
    if notification_token is not None and notification_token > sync_token:
        raise ValueError("notification sync token is newer than the downloaded feed")

    table_name = os.environ.get("LOCK_TABLE", "")
    if not NAME_RE.fullmatch(table_name):
        raise ValueError("LOCK_TABLE is invalid")
    table = boto3.resource("dynamodb", region_name=os.environ.get("AWS_REGION", "us-east-1")).Table(table_name)
    owner = getattr(context, "aws_request_id", None) or str(uuid.uuid4())
    now = int(time.time())
    if not _acquire_lock(table, owner, now):
        logger.info(json.dumps({"event": "sync_skipped", "reason": "lease_held"}, separators=(",", ":")))
        return {"ok": True, "skipped": True}

    changed = 0
    try:
        for target in targets:
            client = boto3.client("lightsail", region_name=target["region"])
            if _sync_target(client, target["instance"], prefixes):
                changed += 1
        logger.info(json.dumps({
            "event": "sync_complete",
            "target_count": len(targets),
            "changed_count": changed,
            "prefix_count": len(prefixes),
            "sync_token": sync_token,
        }, separators=(",", ":")))
        return {
            "ok": True,
            "target_count": len(targets),
            "changed_count": changed,
            "prefix_count": len(prefixes),
            "sync_token": sync_token,
        }
    finally:
        _release_lock(table, owner)
