// A device's role — what the stored facts say it does in the network — as
// the server derives it (internal/devclass ClassifyRole). The role decides
// whether flow export may be set up on a device or accepted from it; the
// reason next to it (Device.role_reason) says which facts it rests on.

import type { DeviceRole } from "@/lib/api";

export const ROLE_LABEL: Record<DeviceRole, string> = {
  router: "router",
  "weak-router": "weak router",
  switch: "switch",
  "ap-cpe": "access point / CPE",
  unknown: "unknown role",
};

// roleLabel: "" for a missing or unrecognised role.
export function roleLabel(role: string | null | undefined): string {
  return (role && ROLE_LABEL[role as DeviceRole]) || "";
}
