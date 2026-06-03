"use client";

import { Suspense, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { Network } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";

const INVALID_LINK_MESSAGE =
  "This reset link is invalid or has expired. Request a new one.";

function ResetPasswordForm() {
  const searchParams = useSearchParams();
  const router = useRouter();
  const token = searchParams.get("token") ?? "";

  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  // Missing token: render the generic invalid-link UI and never call the API.
  if (!token) {
    return (
      <ResetCard>
        <p className="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">
          {INVALID_LINK_MESSAGE}
        </p>
        <Button
          type="button"
          variant="ghost"
          className="mt-4 w-full text-xs"
          onClick={() => router.push("/forgot-password")}
        >
          Request a new link
        </Button>
      </ResetCard>
    );
  }

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");

    if (password.length < 8) {
      setError("Password must be at least 8 characters.");
      return;
    }
    if (password !== confirm) {
      setError("Passwords do not match.");
      return;
    }

    setLoading(true);
    try {
      await api.auth.performReset(token, password);
      // Never store any token here — redirect to log in fresh.
      router.push("/login?reset=1");
    } catch {
      // Collapse every server failure into the same generic message.
      setError(INVALID_LINK_MESSAGE);
      setLoading(false);
    }
  };

  return (
    <ResetCard>
      <form onSubmit={handleSubmit} className="space-y-4">
        <div className="space-y-2">
          <Label htmlFor="password">New password</Label>
          <Input
            id="password"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            autoFocus
            minLength={8}
          />
        </div>
        <div className="space-y-2">
          <Label htmlFor="confirm">Confirm new password</Label>
          <Input
            id="confirm"
            type="password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            required
            minLength={8}
          />
        </div>
        {error && <p className="text-sm text-destructive">{error}</p>}
        <Button type="submit" className="w-full" disabled={loading}>
          {loading ? "..." : "Set new password"}
        </Button>
        <Button
          type="button"
          variant="ghost"
          className="w-full text-xs"
          onClick={() => router.push("/login")}
        >
          Back to Sign In
        </Button>
      </form>
    </ResetCard>
  );
}

function ResetCard({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex min-h-screen items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader className="text-center">
          <div className="mx-auto mb-2 flex h-12 w-12 items-center justify-center rounded-lg bg-primary text-primary-foreground">
            <Network className="h-6 w-6" />
          </div>
          <CardTitle className="text-xl">Choose a new password</CardTitle>
          <CardDescription>Set a new password for your account.</CardDescription>
        </CardHeader>
        <CardContent>{children}</CardContent>
      </Card>
    </div>
  );
}

export default function ResetPasswordPage() {
  return (
    <Suspense>
      <ResetPasswordForm />
    </Suspense>
  );
}
