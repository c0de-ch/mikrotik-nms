"use client";

import { createContext, useContext, useEffect, useState, useCallback, type ReactNode } from "react";
import { api } from "@/lib/api";

interface AuthState {
  token: string | null;
  user: { id: string; username: string; role: string } | null;
  loading: boolean;
}

interface AuthContextValue extends AuthState {
  login: (username: string, password: string) => Promise<void>;
  setup: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

function saveTokens(accessToken: string, refreshToken: string) {
  localStorage.setItem("access_token", accessToken);
  localStorage.setItem("refresh_token", refreshToken);
}

function clearTokens() {
  localStorage.removeItem("access_token");
  localStorage.removeItem("refresh_token");
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>({
    token: null,
    user: null,
    loading: true,
  });

  const checkAuth = useCallback(async () => {
    const savedToken = localStorage.getItem("access_token");
    if (!savedToken) {
      setState({ token: null, user: null, loading: false });
      return;
    }

    // Try using saved access token
    try {
      const user = await api.auth.me(savedToken);
      setState({ token: savedToken, user, loading: false });
      return;
    } catch {
      // Access token expired — try refresh
    }

    const refreshToken = localStorage.getItem("refresh_token");
    if (!refreshToken) {
      clearTokens();
      setState({ token: null, user: null, loading: false });
      return;
    }

    try {
      const tokens = await api.auth.refreshWithToken(refreshToken);
      saveTokens(tokens.access_token, tokens.refresh_token);
      const user = await api.auth.me(tokens.access_token);
      setState({ token: tokens.access_token, user, loading: false });
    } catch {
      clearTokens();
      setState({ token: null, user: null, loading: false });
    }
  }, []);

  useEffect(() => {
    checkAuth();
  }, [checkAuth]);

  const login = async (username: string, password: string) => {
    const tokens = await api.auth.login(username, password);
    saveTokens(tokens.access_token, tokens.refresh_token);
    const user = await api.auth.me(tokens.access_token);
    setState({ token: tokens.access_token, user, loading: false });
  };

  const setup = async (username: string, password: string) => {
    const tokens = await api.auth.setup(username, password);
    saveTokens(tokens.access_token, tokens.refresh_token);
    const user = await api.auth.me(tokens.access_token);
    setState({ token: tokens.access_token, user, loading: false });
  };

  const logout = async () => {
    if (state.token) {
      try {
        await api.auth.logout(state.token);
      } catch {
        // ignore
      }
    }
    clearTokens();
    setState({ token: null, user: null, loading: false });
  };

  return (
    <AuthContext.Provider value={{ ...state, login, setup, logout }}>
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within AuthProvider");
  return ctx;
}
