// Device status presentation — a single source of truth so the
// green (online) / red (offline) / gray (not responding) triad stays
// consistent across the whole app.
//
// Any status that isn't "online" or "offline" (e.g. "unknown", or a device
// that's missing polls but still inside the offline grace window) is treated
// as "not responding" and rendered gray.

export function deviceStatusLabel(status: string): string {
  switch (status) {
    case "online":
      return "online";
    case "offline":
      return "offline";
    default:
      return "not responding";
  }
}

// Tailwind classes for a status Badge. Pair with <Badge variant="outline">.
export function deviceStatusBadgeClass(status: string): string {
  switch (status) {
    case "online":
      return "border-transparent bg-green-500/15 text-green-600 dark:text-green-400";
    case "offline":
      return "border-transparent bg-red-500/15 text-red-600 dark:text-red-400";
    default:
      return "border-transparent bg-gray-400/20 text-gray-600 dark:text-gray-400";
  }
}

// Solid fill color for status dots and stacked-bar segments.
export function deviceStatusColor(status: string): string {
  switch (status) {
    case "online":
      return "bg-green-500";
    case "offline":
      return "bg-red-500";
    default:
      return "bg-gray-400";
  }
}
