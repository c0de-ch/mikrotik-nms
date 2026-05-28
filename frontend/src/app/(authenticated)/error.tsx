"use client";

import { useEffect } from "react";
import { Button } from "@/components/ui/button";

// Route-level error boundary for the authenticated app. A render error in any
// page renders this instead of unmounting the whole layout to a blank screen.
export default function Error({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    console.error(error);
  }, [error]);

  return (
    <div className="flex min-h-[60vh] flex-col items-center justify-center gap-4 p-8 text-center">
      <h2 className="text-lg font-semibold">Something went wrong</h2>
      <p className="max-w-md text-sm text-muted-foreground">
        {error.message || "An unexpected error occurred while rendering this page."}
      </p>
      <Button onClick={reset}>Try again</Button>
    </div>
  );
}
