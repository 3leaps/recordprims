#!/usr/bin/env python3
"""Portable RFC3339 instant helper for contract: agent-wait/v0
and contract: service-job/v0.

Standard-library only. No GNU or BSD date(1). The helper is the
fail-closed grammar for security-sensitive ordering: it does not
delegate acceptance to datetime.fromisoformat.

Accepted profile (RFC 3339 §5.6 date-time):
  YYYY-MM-DD<T|t>hh:mm:ss[.fraction](Z|z|±hh:mm)

Rejected ISO 8601 forms that are not this profile include basic
date/time, week dates, ordinal dates, missing seconds, a space
separator, and numeric offsets without a colon.

Leap-second posture: second 60 is rejected. The helper does not
clamp 23:59:60 to 23:59:59 or roll it into the next day. A leap
second is unparseable and fails closed.
"""

from __future__ import annotations

import re
import sys
from datetime import datetime, timedelta, timezone

# Extended RFC 3339 date-time only. Seconds are required. Numeric
# offsets must include a colon. Fraction uses a period, not a comma.
_RFC3339 = re.compile(
    r"^(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})"
    r"[Tt]"
    r"(?P<hour>\d{2}):(?P<minute>\d{2}):(?P<second>\d{2})"
    r"(?P<frac>\.\d+)?"
    r"(?:(?P<zulu>[Zz])|(?P<sign>[+-])(?P<off_h>\d{2}):(?P<off_m>\d{2}))$"
)


def parse_rfc3339(text: str) -> datetime:
    if not isinstance(text, str) or not text.strip() or text != text.strip():
        raise ValueError("timestamp must be a non-empty RFC3339 string")
    match = _RFC3339.fullmatch(text)
    if match is None:
        raise ValueError(f"unparseable RFC3339 timestamp: {text!r}")

    year = int(match.group("year"))
    month = int(match.group("month"))
    day = int(match.group("day"))
    hour = int(match.group("hour"))
    minute = int(match.group("minute"))
    second = int(match.group("second"))
    if hour > 23 or minute > 59:
        raise ValueError(f"unparseable RFC3339 timestamp: {text!r}")
    if second == 60:
        raise ValueError(f"leap second is not admitted: {text!r}")
    if second > 60:
        raise ValueError(f"unparseable RFC3339 timestamp: {text!r}")

    microsecond = 0
    frac = match.group("frac")
    if frac is not None:
        digits = (frac[1:] + "000000")[:6]
        microsecond = int(digits)

    if match.group("zulu") is not None:
        tzinfo = timezone.utc
    else:
        off_h = int(match.group("off_h"))
        off_m = int(match.group("off_m"))
        if off_h > 23 or off_m > 59:
            raise ValueError(f"unparseable RFC3339 timestamp: {text!r}")
        offset = timedelta(hours=off_h, minutes=off_m)
        if match.group("sign") == "-":
            offset = -offset
        tzinfo = timezone(offset)

    try:
        parsed = datetime(
            year, month, day, hour, minute, second, microsecond, tzinfo=tzinfo
        )
    except ValueError as exc:
        raise ValueError(f"unparseable RFC3339 timestamp: {text!r}") from exc
    return parsed.astimezone(timezone.utc)


def epoch_seconds(text: str) -> int:
    return int(parse_rfc3339(text).timestamp())


def compare(left: str, right: str) -> int:
    a = parse_rfc3339(left)
    b = parse_rfc3339(right)
    if a < b:
        return -1
    if a > b:
        return 1
    return 0


def self_test() -> None:
    failures: list[str] = []

    def expect_reject(label: str, text: str) -> None:
        try:
            parse_rfc3339(text)
        except ValueError:
            return
        failures.append(f"{label}: expected reject of {text!r}")

    def expect_accept(label: str, text: str) -> None:
        try:
            parse_rfc3339(text)
        except ValueError as exc:
            failures.append(f"{label}: expected accept of {text!r}: {exc}")

    expect_reject("empty", "")
    expect_reject("whitespace", "   ")
    expect_reject("garbage", "not-a-timestamp")
    expect_reject("date-only", "2026-08-15")
    expect_reject("naive", "2026-08-15T17:00:00")
    expect_reject("space-separator", "2026-08-15 17:00:00Z")
    expect_reject("basic-date-time", "20260815T170000Z")
    expect_reject("week-date", "2026-W33-6T17:00:00Z")
    expect_reject("ordinal-date", "2026-227T17:00:00Z")
    expect_reject("missing-seconds", "2026-08-15T17:00Z")
    expect_reject("offset-without-colon", "2026-08-15T17:00:00+0000")
    expect_reject("comma-fraction", "2026-08-15T17:00:00,123Z")
    expect_reject("invalid-calendar", "2026-02-30T17:00:00Z")
    expect_reject("hour-24", "2026-08-15T24:00:00Z")
    expect_reject("leap-second-utc", "2016-12-31T23:59:60Z")
    expect_reject("leap-second-offset", "2016-12-31T23:59:60+00:00")

    expect_accept("zulu", "2026-08-15T17:00:00Z")
    expect_accept("lowercase", "2026-08-15t17:00:00z")
    expect_accept("numeric-offset", "2026-08-15T17:00:00+00:00")
    expect_accept("negative-offset", "2026-08-15T13:00:00-04:00")
    expect_accept("fractional-seconds", "2026-08-15T17:00:00.123Z")

    if epoch_seconds("1970-01-01T00:00:00Z") != 0:
        failures.append("epoch 1970-01-01T00:00:00Z must be 0")
    if epoch_seconds("1970-01-01T00:00:01Z") != 1:
        failures.append("epoch 1970-01-01T00:00:01Z must be 1")
    if epoch_seconds("1970-01-01T00:00:00+00:00") != 0:
        failures.append("epoch +00:00 must match Z")
    if epoch_seconds("2026-08-15T17:00:00.123Z") != epoch_seconds(
        "2026-08-15T17:00:00Z"
    ):
        failures.append("fractional seconds must not change integer epoch")

    if compare("1970-01-01T00:00:00Z", "1970-01-01T00:00:00+00:00") != 0:
        failures.append("equality: Z vs +00:00")
    if compare("1970-01-01T01:00:00+01:00", "1970-01-01T00:00:00Z") != 0:
        failures.append("equality: +01:00 vs Z")
    if compare("2026-08-15T17:00:00+00:00", "2026-08-15T17:00:00Z") != 0:
        failures.append("equality: offset-equivalent +00:00 vs Z")
    if compare("1970-01-01T00:00:00Z", "1970-01-01T00:00:01Z") != -1:
        failures.append("before: 00:00:00Z < 00:00:01Z")
    if compare("1970-01-01T00:00:01Z", "1970-01-01T00:00:00Z") != 1:
        failures.append("after: 00:00:01Z > 00:00:00Z")
    if compare("2026-08-15T17:00:00Z", "2026-08-15T17:00:00Z") != 0:
        failures.append("equality: identical instants")
    if compare("2026-08-15T17:00:00.100Z", "2026-08-15T17:00:00Z") != 1:
        failures.append("after: fractional second is later")
    if compare("2026-08-15T16:00:10Z", "2026-08-15T17:00:00+01:00") != 1:
        failures.append("after: later instant despite earlier lexical string")

    if failures:
        raise ValueError("; ".join(failures))


def main(argv: list[str]) -> int:
    if len(argv) == 2 and argv[1] == "--self-test":
        try:
            self_test()
        except ValueError as exc:
            sys.stderr.write(f"rfc3339-instant self-test failed: {exc}\n")
            return 1
        return 0
    if len(argv) == 3 and argv[1] == "--epoch":
        try:
            sys.stdout.write(f"{epoch_seconds(argv[2])}\n")
        except ValueError as exc:
            sys.stderr.write(f"rfc3339-instant: {exc}\n")
            return 1
        return 0
    if len(argv) == 4 and argv[1] == "--compare":
        try:
            sys.stdout.write(f"{compare(argv[2], argv[3])}\n")
        except ValueError as exc:
            sys.stderr.write(f"rfc3339-instant: {exc}\n")
            return 1
        return 0
    sys.stderr.write(
        "usage: rfc3339-instant.py --epoch TIMESTAMP\n"
        "       rfc3339-instant.py --compare A B\n"
        "       rfc3339-instant.py --self-test\n"
    )
    return 2


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
