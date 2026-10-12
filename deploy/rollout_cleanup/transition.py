"""Explicit configuration-transition reconciliation; never rewrite an origin."""
from __future__ import annotations

import copy
import hashlib
import os
import stat
import uuid
from pathlib import Path
from typing import Any


def reconcile(job_id: str, previous_config: str, expected_config_sha256: str,
              expected_binary_sha256: str, *, runtime=None) -> dict[str, Any]:
    # Import at call time to avoid a package initialization cycle.
    from . import (
        CleanupError, HEX64, _rt, _validate_job_id, _safe_record,
        _registered_release, _identity, _config_hash, _owns_root,
        _block, _revalidate_artifact, _capture_origin, _completion_proof,
        _origin_static_unchanged, _binary_unchanged, atomic_json, _state_json_paths,
    )
    rt = _rt(runtime)
    _validate_job_id(job_id)
    if not HEX64.fullmatch(expected_config_sha256) or not HEX64.fullmatch(expected_binary_sha256):
        raise CleanupError("invalid transition hashes")
    path = rt.state_dir / (job_id + ".json")
    job = _safe_record(path, rt)
    if job.get("id") != job_id:
        raise CleanupError("transition job identity mismatch")
    if job.get("status") == "superseded":
        successor_id = job.get("replaced_by", "")
        _validate_job_id(successor_id)
        successor = _safe_record(rt.state_dir / (successor_id + ".json"), rt)
        if (successor.get("status") != "completed" or successor.get("replaces") != job_id
                or successor.get("origin", {}).get("config_sha256") != expected_config_sha256
                or successor.get("binary_sha256") != expected_binary_sha256):
            raise CleanupError("transition successor binding failed")
        return {"id": job_id, "status": "superseded", "replacement_id": successor_id}
    if job.get("status") != "begun":
        raise CleanupError("only an incomplete registration can be reconciled")
    release = _registered_release(job, rt)
    origin = job["origin"]
    previous = Path(previous_config)
    if not previous.is_absolute() or previous.parent != release:
        raise CleanupError("previous configuration must be retained in the registered release")
    info = os.lstat(previous)
    if (not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or not _owns_root(info, rt)
            or stat.S_IMODE(info.st_mode) & 0o077):
        raise CleanupError("previous configuration must be a protected, root-owned regular file")
    previous_identity = _identity(previous)
    if _config_hash(previous) != origin["config_sha256"]:
        raise CleanupError("retained previous configuration does not match original receipt")
    if (expected_config_sha256 == origin["config_sha256"]
            or _config_hash(Path(origin["config"])) != expected_config_sha256):
        raise CleanupError("explicit approved configuration hash does not match the transition")

    def verify_previous_evidence():
        if (_identity(previous) != previous_identity
                or _config_hash(previous) != origin["config_sha256"]):
            raise CleanupError("previous configuration changed during reconciliation")
        if _identity(Path(origin["data_dir"]) / "chain.db", allow_dir=True) != origin["chain_db"]:
            raise CleanupError("live chain.db identity changed")
        for height, expected in ((0, origin["genesis_hash"]), (origin["height"], origin["hash"])):
            if _block(origin["api_url"], height, rt)[1] != expected:
                raise CleanupError("original canonical transition evidence changed")
        for item in job["marks"]:
            _revalidate_artifact(item, job, rt)

    verify_previous_evidence()
    original_digest = hashlib.sha256(path.read_bytes()).hexdigest()
    binding = {
        "previous_config_sha256": origin["config_sha256"],
        "approved_config_sha256": expected_config_sha256,
        "previous_receipt_sha256": original_digest,
    }
    # Resume safely if the process died between the two atomic journal writes.
    successor = None
    for candidate_path in _state_json_paths(rt.state_dir):
        candidate = _safe_record(candidate_path, rt)
        if candidate.get("replaces") == job_id:
            if candidate.get("config_transition") != binding or successor is not None:
                raise CleanupError("ambiguous or mismatched transition successor")
            successor = candidate
    if successor is None:
        fresh = _capture_origin(Path(origin["data_dir"]), Path(origin["config"]),
                                origin["service"], origin["api_url"], rt)
        successor = {
            "schema": 1, "id": str(uuid.uuid4()), "created_at": int(rt.clock()),
            "status": "begun", "release": job["release"],
            "release_identity": copy.deepcopy(job["release_identity"]),
            "origin": fresh, "marks": copy.deepcopy(job["marks"]),
            "replaces": job_id, "config_transition": binding,
        }
        # No successor is persisted until all normal completion guards pass.
        successor.update(_completion_proof(successor, expected_binary_sha256, rt))
    else:
        if (successor.get("status") != "completed"
                or successor.get("origin", {}).get("config_sha256") != expected_config_sha256
                or successor.get("binary_sha256") != expected_binary_sha256):
            raise CleanupError("transition successor has not completed")
        _origin_static_unchanged(successor["origin"], rt)
        _binary_unchanged(successor, rt)
    verify_previous_evidence()
    if hashlib.sha256(path.read_bytes()).hexdigest() != original_digest:
        raise CleanupError("original receipt changed during reconciliation")
    atomic_json(rt.state_dir / (successor["id"] + ".json"), successor)
    # Preserve the entire original origin and marks, with an honest terminal state.
    job.update({"status": "superseded", "closed_at": int(rt.clock()),
                "replaced_by": successor["id"], "closure_reason": "verified_config_transition",
                "config_transition": binding})
    atomic_json(path, job)
    return {"id": job_id, "status": "superseded", "replacement_id": successor["id"],
            "replacement_status": "completed"}