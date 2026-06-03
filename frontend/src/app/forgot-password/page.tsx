"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import { Network } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";

// The neutral message shown after every submit — it must never reveal whether
// an account exists for the supplied email.
const NEUTRAL_MESSAGE =
  "If an account exists for that email, we have sent a password reset link.";

export default function ForgotPasswordPage() {
  const [email, setEmail] = useState("");
  const [submitted, setSubmitted] = useState(false);
  const [loading, setLoading] = useState(false);
  const router = useRouter();

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoading(true);
    try {
      await api.auth.requestReset(email);
    } catch {
      // Deliberately ignore errors (including rate-limit 429): the message is
      // the same regardless so we never leak account existence or state.
    } finally {
      setLoading(false);
      setSubmitted(true);
    }
  };

  return (
    <div className="flex min-h-screen items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader className="text-center">
          <div className="mx-auto mb-2 flex h-12 w-12 items-center justify-center rounded-lg bg-primary text-primary-foreground">
            <Network className="h-6 w-6" />
          </div>
          <CardTitle className="text-xl">Reset your password</CardTitle>
          <CardDescription>
            Enter your account email and we&apos;ll send you a reset link.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {submitted ? (
            <div className="space-y-4">
              <p className="rounded-md bg-muted px-3 py-2 text-sm text-muted-foreground">
                {NEUTRAL_MESSAGE}
              </p>
              <Button
                type="button"
                variant="ghost"
                className="w-full text-xs"
                onClick={() => router.push("/login")}
              >
                Back to Sign In
              </Button>
            </div>
          ) : (
            <form onSubmit={handleSubmit} className="space-y-4">
              <div className="space-y-2">
                <Label htmlFor="email">Email</Label>
                <Input
                  id="email"
                  type="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  required
                  autoFocus
                />
              </div>
              <Button type="submit" className="w-full" disabled={loading}>
                {loading ? "..." : "Send reset link"}
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
          )}
        </CardContent>
      </Card>
    </div>
  );
}
