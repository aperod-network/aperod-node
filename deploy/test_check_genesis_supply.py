#!/usr/bin/env python3
"""Offline CLI regressions; historical team fixtures never change live policy."""
import copy
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().with_name("check-genesis-supply.sh")
CURRENT_DISTRIBUTION = SCRIPT.parents[2] / "lib/db/genesis-distribution.json"
ANSI = re.compile(r"\x1b\[[0-9;]*m")


class GenesisSupplyPolicyTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="genesis-policy-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.config = self.root / "node.yaml"
        self.config.write_text('genesis:\n  file: "genesis.yaml"\n')
        self.genesis = self.root / "genesis.yaml"
        self.genesis.write_text("""initial_supply: 10000000000
timestamp: 1735689600
allocations:
  - address: ""
    amount: 900000000000000000
    label: "Public"
    vesting:
      type: immediate
  - address: ""
    amount: 100000000000000000
    label: "Development Fund"
    vesting:
      type: cliff_linear
      cliff_seconds: 31536000
      vest_seconds: 126144000
""")
        self.distribution = self.root / "policy.json"
        # Representative cancelled model, NOT current distribution data.
        primary = {
            "address": "HISTORICAL_TEST_ONLY", "label": "Development Fund",
            "category": "team", "amount_apro": 1_000_000_000,
            "vesting_type": "cliff_linear", "cliff_seconds": 31536000,
            "vest_seconds": 315360000,
        }
        self.historical = [
            {"label": "Public", "category": "community", "amount_apro": 9_000_000_000,
             "vesting_type": "immediate"},
            primary,
        ] + [
            {**primary, "label": f"Historical tranche {i + 1}",
             "amount_apro": 84_910_000 if i == 9 else 84_900_000,
             "unlock_date_utc": f"{2027 + i}-02-10"}
            for i in range(10)
        ]  # No is_schedule_entry flags: exercise the same dated fallback as the API.
        self.write_policy(self.historical)

    def write_policy(self, policy):
        self.distribution.write_text(json.dumps(policy))

    def run_audit(self, at="2026-08-14", distribution=None, script=SCRIPT, extra=(), environment=None):
        args = ["bash", str(script), "--config", str(self.config)]
        if distribution is not False:
            args += ["--distribution", str(distribution or self.distribution)]
        if at:
            args += ["--at", at]
        args += list(extra)
        # No inherited credentials, no service or network calls.
        env = {"PATH": os.environ["PATH"], "LC_ALL": "C", **(environment or {})}
        result = subprocess.run(args, capture_output=True, text=True, env=env, cwd=self.root, timeout=10)
        result.stdout = ANSI.sub("", result.stdout)
        return result

    def amounts(self, result, label):
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        line = next(row for row in result.stdout.splitlines() if row.strip().startswith(label + " "))
        return re.findall(r"\d[\d,]*\.\d{4}", line)[:3]

    def test_historical_cliff_is_not_interpolated_from_four_year_yaml(self):
        result = self.run_audit()
        self.assertEqual(self.amounts(result, "Development Fund"),
                         ["1,000,000,000.0000", "150,990,000.0000", "849,010,000.0000"])
        self.assertIn("dated-tranches", result.stdout)
        self.assertIn("not its legacy continuous schedule", result.stdout)

    def test_first_tranche_unlocks_on_exact_utc_day(self):
        for day, unlocked in [
            ("2027-02-09", "150,990,000.0000"),
            ("2027-02-10", "235,890,000.0000"),
            ("2027-02-11", "235,890,000.0000"),
        ]:
            with self.subTest(day=day):
                self.assertEqual(self.amounts(self.run_audit(at=day), "Development Fund")[1], unlocked)

    def test_cliff_and_last_tranche_boundaries(self):
        for day, unlocked in [
            ("2025-12-31", "0.0000"),
            ("2026-01-01", "150,990,000.0000"),
            ("2036-02-10", "1,000,000,000.0000"),
        ]:
            with self.subTest(day=day):
                self.assertEqual(self.amounts(self.run_audit(at=day), "Development Fund")[1], unlocked)

    def test_changing_json_tranche_amount_changes_the_cliff_not_a_hardcoded_value(self):
        self.historical[2]["amount_apro"] -= 1_000_000
        self.write_policy(self.historical)
        self.assertEqual(self.amounts(self.run_audit(), "Development Fund")[1], "151,990,000.0000")

    def test_current_cancelled_fund_is_fully_available_without_restoring_team_tranches(self):
        before = self.genesis.read_bytes()
        for day in ["2026-08-14", "2027-02-10"]:
            result = self.run_audit(at=day, distribution=CURRENT_DISTRIBUTION)
            self.assertEqual(self.amounts(result, "Community & Ecosystem"),
                             ["1,000,000,000.0000", "1,000,000,000.0000", "0.0000"])
            self.assertIn("team entitlement cancelled", result.stdout)
            self.assertIn("NOT verified circulating or spendable UTXOs", result.stdout)
            self.assertIn("PASS", result.stdout)
        self.assertEqual(self.genesis.read_bytes(), before)
        self.assertFalse(any(e.get("category") == "team" for e in json.loads(CURRENT_DISTRIBUTION.read_text())))

    def test_removed_tranches_do_not_silently_fall_back_to_continuous_vesting(self):
        self.write_policy(self.historical[:2])
        result = self.run_audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("refusing continuous interpolation", result.stderr)

    def test_invalid_dates_amounts_and_double_counted_schedules_fail_explicitly(self):
        for field, value in [("unlock_date_utc", "2027-02-30"), ("amount_apro", -1),
                             ("amount_apro", 1.5), ("unlock_date_utc", "")]:
            with self.subTest(field=field, value=value):
                policy = copy.deepcopy(self.historical)
                policy[2][field] = value
                self.write_policy(policy)
                result = self.run_audit()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("ERROR:", result.stderr)

    def test_missing_policy_does_not_assume_a_fully_unlocked_team_allocation(self):
        result = self.run_audit(distribution=self.root / "missing.json")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("distribution JSON not found", result.stderr)

    def test_policy_overlay_cannot_hide_original_genesis_total_mismatch(self):
        self.genesis.write_text(self.genesis.read_text().replace("initial_supply: 10000000000",
                                                               "initial_supply: 9999999999"))
        result = self.run_audit(distribution=CURRENT_DISTRIBUTION)
        self.assertEqual(result.returncode, 1)
        self.assertIn("DO NOT match", result.stdout)

    def test_installed_script_resolves_policy_from_explicit_checkout_root(self):
        installed = self.root / "installed" / "usr/local/bin" / SCRIPT.name
        installed.parent.mkdir(parents=True)
        shutil.copyfile(SCRIPT, installed)
        checkout = self.root / "checkout"
        policy = checkout / "lib/db/genesis-distribution.json"
        policy.parent.mkdir(parents=True)
        shutil.copyfile(CURRENT_DISTRIBUTION, policy)
        result = self.run_audit(distribution=False, script=installed,
                                environment={"APEROD_DIR": str(checkout)})
        self.assertEqual(self.amounts(result, "Community & Ecosystem")[1], "1,000,000,000.0000")

    def test_checkout_default_policy_does_not_depend_on_working_directory(self):
        result = self.run_audit(distribution=False)
        self.assertEqual(self.amounts(result, "Community & Ecosystem")[1], "1,000,000,000.0000")

    def test_historical_snapshot_cannot_mix_in_live_api_counters(self):
        result = self.run_audit(extra=("--api-url", "http://127.0.0.1:1"))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("offline-only", result.stderr)

    def test_missing_genesis_time_does_not_present_unknown_totals_as_zero(self):
        self.genesis.write_text(self.genesis.read_text().replace("timestamp: 1735689600", "timestamp: 0"))
        result = self.run_audit()
        self.assertEqual(result.returncode, 0, result.stderr)
        total = next(line for line in result.stdout.splitlines() if "GENESIS TOTAL" in line)
        self.assertEqual(total.count("unknown"), 2)

    def test_identity_mismatch_is_not_treated_as_cancellation_of_another_wallets_lock(self):
        self.genesis.write_text(self.genesis.read_text().replace('address: ""', 'address: "OTHER_WALLET"'))
        result = self.run_audit(distribution=CURRENT_DISTRIBUTION)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cannot be uniquely reconciled", result.stderr)

    def test_discrete_utc_boundaries_are_independent_of_process_timezone(self):
        result = self.run_audit(at="2027-02-10", environment={"TZ": "America/Los_Angeles"})
        self.assertEqual(self.amounts(result, "Development Fund")[1], "235,890,000.0000")

    def test_empty_snapshot_option_cannot_silently_mean_today(self):
        result = self.run_audit(at=None, extra=("--at=",))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Missing --at date", result.stderr)


if __name__ == "__main__":
    unittest.main()
