"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";
import { useAuth } from "@/context/auth";
import { AppSidebar } from "@/components/layout/app-sidebar";
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar";
import { api } from "@/lib/api";
import { useVersionWatchdog } from "@/hooks/use-version-watchdog";

export default function AuthenticatedLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  const { token, loading } = useAuth();
  const router = useRouter();

  // Watchdog: poll /health periodically and reload the page if the backend
  // instance_id changes, so the tab self-heals after a deploy without the
  // user having to hit Cmd+Shift+R.
  useVersionWatchdog();

  useEffect(() => {
    if (!loading && !token) {
      router.push("/login");
    }
  }, [loading, token, router]);

  // Load dark mode preference from backend settings
  useEffect(() => {
    if (!token) return;
    api.settings.get(token).then((settings) => {
      if (settings.dark_mode === "true") {
        document.documentElement.classList.add("dark");
      } else {
        document.documentElement.classList.remove("dark");
      }
    }).catch(() => {});
  }, [token]);

  if (loading) {
    return (
      <div className="flex min-h-screen items-center justify-center">
        <div className="text-muted-foreground">Loading...</div>
      </div>
    );
  }

  if (!token) return null;

  return (
    <SidebarProvider>
      <AppSidebar />
      <SidebarInset>
        <main className="flex-1 p-6">{children}</main>
      </SidebarInset>
    </SidebarProvider>
  );
}
