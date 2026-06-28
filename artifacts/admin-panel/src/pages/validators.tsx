import { useEffect, useState } from "react";
import { AdminLayout } from "@/components/AdminLayout";
import { AuthGuard } from "@/components/AuthGuard";
import { validatorsApi } from "@/lib/api";
import { useT } from "@/lib/i18n";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { useToast } from "@/hooks/use-toast";
import { Trash2, Plus, RefreshCw, ChevronDown, ChevronUp, WifiOff, Wifi, Power, ShieldCheck } from "lucide-react";

interface Validator {
  id: string;
  pubKey: string;
  alias: string;
  endpoint: string;
  address: string | null;
  online: boolean;
  forceActive: boolean;
  addedAt: number;
  lastSeen: number | null;
}

interface TransitionEvent {
  alias: string;
  transition: "offline" | "online";
  at: number;
}

function ValidatorHistoryPanel({ alias }: { alias: string }) {
  const { t } = useT();
  const [events, setEvents] = useState<TransitionEvent[] | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    validatorsApi
      .history(alias)
      .then((r: { history: TransitionEvent[] }) => setEvents(r.history))
      .catch(() => setEvents([]))
      .finally(() => setLoading(false));
  }, [alias]);

  if (loading) {
    return (
      <div className="mt-2 px-3 py-2 rounded-md bg-muted/30 text-xs text-muted-foreground">
        {t.validators.loadingHistory}
      </div>
    );
  }

  if (!events || events.length === 0) {
    return (
      <div className="mt-2 px-3 py-2 rounded-md bg-muted/30 text-xs text-muted-foreground">
        {t.validators.noHistory}
      </div>
    );
  }

  return (
    <div className="mt-2 rounded-md border bg-muted/20 divide-y divide-border overflow-hidden">
      {events.map((ev, i) => (
        <div key={i} className="flex items-center gap-2.5 px-3 py-2">
          {ev.transition === "offline" ? (
            <WifiOff className="w-3.5 h-3.5 text-red-500 shrink-0" />
          ) : (
            <Wifi className="w-3.5 h-3.5 text-green-500 shrink-0" />
          )}
          <span className={`text-xs font-medium ${ev.transition === "offline" ? "text-red-600 dark:text-red-400" : "text-green-600 dark:text-green-400"}`}>
            {ev.transition === "offline" ? t.validators.wentOffline : t.validators.cameOnline}
          </span>
          <span className="text-xs text-muted-foreground ml-auto">
            {new Date(ev.at).toLocaleString("ru-RU", { timeZone: "Europe/Moscow", day: "2-digit", month: "short", year: "numeric", hour: "2-digit", minute: "2-digit", hour12: false })} МСК
          </span>
        </div>
      ))}
    </div>
  );
}

interface StakeInfo {
  balance_apr: number;
  min_stake_apr: number;
  threshold_met: boolean;
  node_reachable: boolean;
  loading: boolean;
}

export default function ValidatorsPage() {
  const { toast } = useToast();
  const { t } = useT();
  const [validators, setValidators] = useState<Validator[]>([]);
  const [loading, setLoading] = useState(true);
  const [pubKey, setPubKey] = useState("");
  const [alias, setAlias] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [adding, setAdding] = useState(false);
  const [expandedHistory, setExpandedHistory] = useState<Set<string>>(new Set());
  const [stakeInfo, setStakeInfo] = useState<Record<string, StakeInfo>>({});
  const [editingAddressId, setEditingAddressId] = useState<string | null>(null);
  const [editAddressValue, setEditAddressValue] = useState("");

  const checkStake = async (id: string) => {
    setStakeInfo((prev) => ({ ...prev, [id]: { ...(prev[id] ?? { balance_apr: 0, min_stake_apr: 100_000, threshold_met: false, node_reachable: true }), loading: true } }));
    try {
      const r = await validatorsApi.getBalance(id);
      setStakeInfo((prev) => ({ ...prev, [id]: { ...r, loading: false } }));
    } catch {
      setStakeInfo((prev) => ({ ...prev, [id]: { balance_apr: 0, min_stake_apr: 100_000, threshold_met: false, node_reachable: false, loading: false } }));
    }
  };

  const load = () => {
    setLoading(true);
    return validatorsApi
      .list()
      .then((r: { validators: Validator[] }) => setValidators(r.validators))
      .catch((e: Error) => toast({ title: t.common.error, description: e.message, variant: "destructive" }))
      .finally(() => setLoading(false));
  };

  useEffect(() => { void load(); }, []);

  const handleAdd = async () => {
    if (!pubKey.trim()) return;
    setAdding(true);
    try {
      await validatorsApi.add(pubKey.trim(), alias.trim() || `validator-${Date.now()}`, endpoint.trim());
      setPubKey("");
      setAlias("");
      setEndpoint("");
      await load();
      toast({ title: t.common.added, description: t.validators.addedSuccess });
    } catch (e: unknown) {
      toast({ title: t.common.error, description: (e as Error).message, variant: "destructive" });
    } finally {
      setAdding(false);
    }
  };

  const handleToggleOnline = async (id: string, currentOnline: boolean) => {
    try {
      await validatorsApi.setOnline(id, !currentOnline);
      await load();
      toast({
        title: !currentOnline ? t.validators.activate : t.validators.deactivate,
        description: !currentOnline ? t.validators.activateSuccess : t.validators.deactivateSuccess,
      });
    } catch (e: unknown) {
      toast({ title: t.common.error, description: (e as Error).message, variant: "destructive" });
    }
  };

  const handleRemove = async (id: string, name: string) => {
    try {
      await validatorsApi.remove(id);
      setExpandedHistory((prev) => {
        const next = new Set(prev);
        next.delete(name);
        return next;
      });
      await load();
      toast({ title: t.common.removed, description: `${name} ${t.validators.removedSuccess}` });
    } catch (e: unknown) {
      toast({ title: t.common.error, description: (e as Error).message, variant: "destructive" });
    }
  };

  const handleSaveAddress = async (id: string) => {
    const address = editAddressValue.trim() || null;
    try {
      await validatorsApi.setAddress(id, address);
      setEditingAddressId(null);
      await load();
      toast({ title: "Адрес обновлён", description: "Кошелёк валидатора сохранён." });
    } catch (e: unknown) {
      toast({ title: t.common.error, description: (e as Error).message, variant: "destructive" });
    }
  };

  const toggleHistory = (alias: string) => {
    setExpandedHistory((prev) => {
      const next = new Set(prev);
      if (next.has(alias)) {
        next.delete(alias);
      } else {
        next.add(alias);
      }
      return next;
    });
  };

  const online = validators.filter((v) => v.online).length;

  return (
    <AuthGuard>
      <AdminLayout>
        <div className="space-y-6">
          <div className="flex items-center justify-between">
            <div>
              <h1 className="text-2xl font-bold tracking-tight">{t.validators.title}</h1>
              <p className="text-sm text-muted-foreground mt-0.5">
                {online}/{validators.length} {t.validators.onlineOf}
              </p>
            </div>
            <Button variant="outline" size="sm" onClick={load} disabled={loading}>
              <RefreshCw className={`w-4 h-4 mr-1.5 ${loading ? "animate-spin" : ""}`} />
              {t.common.refresh}
            </Button>
          </div>

          <Card>
            <CardHeader>
              <CardTitle className="text-base">{t.validators.addValidator}</CardTitle>
              <CardDescription>{t.validators.addValidatorSub}</CardDescription>
            </CardHeader>
            <CardContent>
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                <div className="space-y-1.5">
                  <Label htmlFor="pk">{t.validators.publicKey}</Label>
                  <Input
                    id="pk"
                    className="font-mono text-xs"
                    placeholder={t.validators.publicKeyPh}
                    value={pubKey}
                    onChange={(e) => setPubKey(e.target.value)}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="ep">{t.validators.endpoint}</Label>
                  <Input
                    id="ep"
                    className="font-mono text-xs"
                    placeholder={t.validators.endpointPh}
                    value={endpoint}
                    onChange={(e) => setEndpoint(e.target.value)}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="al">{t.validators.alias}</Label>
                  <Input id="al" placeholder={t.validators.aliasPh} value={alias} onChange={(e) => setAlias(e.target.value)} />
                </div>
                <div className="sm:self-end">
                  <Button onClick={handleAdd} disabled={adding || pubKey.length < 32}>
                    <Plus className="w-4 h-4 mr-1.5" />
                    {adding ? t.common.adding : t.common.add}
                  </Button>
                </div>
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="text-base">{t.validators.activeValidators}</CardTitle>
            </CardHeader>
            <CardContent>
              {loading ? (
                <div className="flex items-center justify-center h-24 text-muted-foreground text-sm">{t.common.loading}</div>
              ) : validators.length === 0 ? (
                <div className="flex items-center justify-center h-24 text-muted-foreground text-sm">{t.validators.noValidators}</div>
              ) : (
                <div className="space-y-2">
                  {validators.map((v) => (
                    <div key={v.id} className="rounded-lg border bg-card">
                      <div className="flex items-center gap-3 p-3 hover:bg-muted/20 transition-colors">
                        <div className={`w-2 h-2 rounded-full shrink-0 ${v.online && v.lastSeen !== null && Date.now() - v.lastSeen < 300_000 ? "bg-green-500 animate-pulse" : "bg-red-500"}`} />
                        <div className="flex-1 min-w-0">
                          <div className="flex items-center gap-2">
                            <span className="font-medium text-sm">{v.alias}</span>
                            <Badge variant={v.online && v.lastSeen !== null && Date.now() - v.lastSeen < 300_000 ? "default" : "secondary"} className="text-xs">
                              {v.online && v.lastSeen !== null && Date.now() - v.lastSeen < 300_000 ? t.common.online : t.common.offline}
                            </Badge>
                          </div>
                          <p className="text-xs text-muted-foreground font-mono truncate mt-0.5">{v.pubKey}</p>
                          {v.endpoint && (
                            <p className="text-xs font-mono text-blue-400/80 truncate mt-0.5">⬡ {v.endpoint}</p>
                          )}
                          {editingAddressId === v.id ? (
                            <div className="flex items-center gap-1.5 mt-1">
                              <Input
                                className="h-6 text-xs font-mono px-2 py-0"
                                placeholder="APR адрес кошелька…"
                                value={editAddressValue}
                                onChange={(e) => setEditAddressValue(e.target.value)}
                                onKeyDown={(e) => {
                                  if (e.key === "Enter") void handleSaveAddress(v.id);
                                  if (e.key === "Escape") setEditingAddressId(null);
                                }}
                                autoFocus
                              />
                              <Button size="sm" className="h-6 px-2 text-xs" onClick={() => void handleSaveAddress(v.id)}>
                                ✓
                              </Button>
                              <Button size="sm" variant="ghost" className="h-6 px-2 text-xs" onClick={() => setEditingAddressId(null)}>
                                ✕
                              </Button>
                            </div>
                          ) : (
                            <button
                              className="flex items-center gap-1 mt-0.5 group text-left w-full"
                              onClick={() => { setEditingAddressId(v.id); setEditAddressValue(v.address ?? ""); }}
                              title="Нажмите чтобы изменить адрес кошелька"
                            >
                              <span className="text-xs font-mono text-muted-foreground/70 truncate">
                                💳 {v.address ? v.address : <span className="italic text-muted-foreground/40">адрес не задан</span>}
                              </span>
                              <span className="text-xs text-muted-foreground/40 group-hover:text-muted-foreground shrink-0 ml-1">✎</span>
                            </button>
                          )}
                          <p className="text-xs text-muted-foreground mt-0.5">
                            {t.validators.addedAt} {new Date(v.addedAt).toLocaleDateString("ru-RU", { timeZone: "Europe/Moscow", day: "numeric", month: "short", year: "numeric" })}
                            {v.lastSeen && ` · ${t.validators.lastSeen} ${new Date(v.lastSeen).toLocaleTimeString("ru-RU", { timeZone: "Europe/Moscow", hour: "2-digit", minute: "2-digit", hour12: false })} МСК`}
                          </p>
                        </div>
                        <Button
                          variant="ghost"
                          size="sm"
                          className="text-muted-foreground shrink-0 gap-1"
                          onClick={() => toggleHistory(v.alias)}
                        >
                          {expandedHistory.has(v.alias) ? (
                            <>
                              <ChevronUp className="w-3.5 h-3.5" />
                              {t.validators.hideHistory}
                            </>
                          ) : (
                            <>
                              <ChevronDown className="w-3.5 h-3.5" />
                              {t.validators.showHistory}
                            </>
                          )}
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          className={`shrink-0 gap-1 ${v.online ? "text-green-600 hover:text-red-500" : "text-muted-foreground hover:text-green-600"}`}
                          onClick={() => handleToggleOnline(v.id, v.online)}
                          title={v.online ? t.validators.deactivate : t.validators.activate}
                        >
                          <Power className="w-3.5 h-3.5" />
                          {v.online ? t.validators.deactivate : t.validators.activate}
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          className="text-muted-foreground hover:text-destructive shrink-0"
                          onClick={() => handleRemove(v.id, v.alias)}
                        >
                          <Trash2 className="w-4 h-4" />
                        </Button>
                      </div>
                      {expandedHistory.has(v.alias) && (
                        <div className="px-3 pb-3">
                          <p className="text-xs font-medium text-muted-foreground mb-1.5">{t.validators.statusHistory}</p>
                          <ValidatorHistoryPanel alias={v.alias} />
                        </div>
                      )}
                      {/* Stake panel */}
                      {v.address && (
                        <div className="px-3 pb-3 border-t">
                          <div className="flex items-center justify-between mt-2 mb-1.5">
                            <span className="text-xs font-medium text-muted-foreground">💰 Стейк</span>
                            <button
                              className="text-xs text-blue-400 hover:text-blue-300 transition-colors"
                              onClick={() => void checkStake(v.id)}
                              disabled={stakeInfo[v.id]?.loading}
                            >
                              {stakeInfo[v.id]?.loading ? "..." : "Проверить"}
                            </button>
                          </div>
                          {stakeInfo[v.id] && !stakeInfo[v.id]!.loading && (
                            <div className="space-y-1.5">
                              <div className="flex items-center justify-between text-xs">
                                <span className={stakeInfo[v.id]!.threshold_met ? "text-green-500 font-medium" : "text-amber-400"}>
                                  {stakeInfo[v.id]!.threshold_met ? "✅ Порог набран" : "⏳ Не хватает стейка"}
                                </span>
                                <span className="text-muted-foreground font-mono">
                                  {stakeInfo[v.id]!.balance_apr.toLocaleString("ru-RU")} / {stakeInfo[v.id]!.min_stake_apr.toLocaleString("ru-RU")} APR
                                </span>
                              </div>
                              <div className="w-full bg-muted rounded-full h-1.5 overflow-hidden">
                                <div
                                  className={`h-1.5 rounded-full transition-all ${stakeInfo[v.id]!.threshold_met ? "bg-green-500" : "bg-amber-400"}`}
                                  style={{ width: `${Math.min(100, (stakeInfo[v.id]!.balance_apr / stakeInfo[v.id]!.min_stake_apr) * 100).toFixed(1)}%` }}
                                />
                              </div>
                              {!stakeInfo[v.id]!.threshold_met && stakeInfo[v.id]!.node_reachable && (
                                <p className="text-xs text-muted-foreground">
                                  Нужно ещё: {(stakeInfo[v.id]!.min_stake_apr - stakeInfo[v.id]!.balance_apr).toLocaleString("ru-RU")} APR
                                </p>
                              )}
                              {!stakeInfo[v.id]!.node_reachable && (
                                <p className="text-xs text-amber-500/80">
                                  ⚠ RPC-нода не отвечала — баланс может быть устаревшим
                                </p>
                              )}
                            </div>
                          )}
                        </div>
                      )}
                    </div>
                  ))}
                </div>
              )}
            </CardContent>
          </Card>
        </div>
      </AdminLayout>
    </AuthGuard>
  );
}
