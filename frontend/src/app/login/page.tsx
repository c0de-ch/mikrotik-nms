"use client";

import { Suspense, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { Network } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useAuth } from "@/context/auth";

function LoginForm() {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [isSetup, setIsSetup] = useState(false);
  const [loading, setLoading] = useState(false);
  const { login, setup } = useAuth();
  const router = useRouter();
  const searchParams = useSearchParams();
  const justReset = searchParams.get("reset") === "1";

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");
    setLoading(true);

    try {
      if (isSetup) {
        await setup(username, password);
      } else {
        await login(username, password);
      }
      router.push("/dashboard");
    } catch (err) {
      setError(err instanceof Error && err.message ? err.message : "Authentication failed");
      // If login fails with specific error, suggest setup
      if (!isSetup && String(err).includes("invalid credentials")) {
        setError("Invalid credentials. If this is a fresh install, click 'First-time Setup' below.");
      }
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="flex min-h-screen items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader className="text-center">
          <div className="mx-auto mb-2 flex h-12 w-12 items-center justify-center rounded-lg bg-primary text-primary-foreground">
            <Network className="h-6 w-6" />
          </div>
          <CardTitle className="text-xl">MikroTik NMS</CardTitle>
          <CardDescription>
            {isSetup ? "Create the initial admin account" : "Sign in to your account"}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {justReset && !isSetup && (
            <p className="mb-4 rounded-md bg-green-500/10 px-3 py-2 text-sm text-green-600 dark:text-green-400">
              Your password was reset — please sign in.
            </p>
          )}
          <form onSubmit={handleSubmit} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="username">Username</Label>
              <Input
                id="username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                required
                autoFocus
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="password">Password</Label>
              <Input
                id="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
              />
            </div>
            {error && (
              <p className="text-sm text-destructive">{error}</p>
            )}
            <Button type="submit" className="w-full" disabled={loading}>
              {loading ? "..." : isSetup ? "Create Admin Account" : "Sign In"}
            </Button>
            {!isSetup && (
              <Button
                type="button"
                variant="ghost"
                className="w-full text-xs"
                onClick={() => router.push("/forgot-password")}
              >
                Forgot password?
              </Button>
            )}
            <Button
              type="button"
              variant="ghost"
              className="w-full text-xs"
              onClick={() => { setIsSetup(!isSetup); setError(""); }}
            >
              {isSetup ? "Back to Sign In" : "First-time Setup"}
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}

export default function LoginPage() {
  return (
    <Suspense>
      <LoginForm />
    </Suspense>
  );
}
