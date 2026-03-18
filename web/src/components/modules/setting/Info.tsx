'use client';

import { useTranslations } from 'next-intl';
import { Info, Tag, Github, AlertTriangle, Download, Loader2, Database, RefreshCw } from 'lucide-react';
import { APP_VERSION, GITHUB_REPO } from '@/lib/info';
import { useLatestInfo, useNowVersion, useUpdateCore } from '@/api/endpoints/update';
import { useSQLiteCheckpoint, useSQLiteStatus } from '@/api/endpoints/setting';
import { Button } from '@/components/ui/button';
import { toast } from '@/components/common/Toast';
import { isOctopusCacheName, isFontCacheName, SW_MESSAGE_TYPE } from '@/lib/sw';

function formatBytes(bytes?: number) {
    if (typeof bytes !== 'number' || Number.isNaN(bytes) || bytes < 0) {
        return '--';
    }
    if (bytes === 0) {
        return '0 B';
    }

    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let value = bytes;
    let unitIndex = 0;

    while (value >= 1024 && unitIndex < units.length - 1) {
        value /= 1024;
        unitIndex += 1;
    }

    const digits = value >= 100 || unitIndex === 0 ? 0 : value >= 10 ? 1 : 2;
    return `${value.toFixed(digits)} ${units[unitIndex]}`;
}

function formatCount(value?: number) {
    if (typeof value !== 'number' || Number.isNaN(value)) {
        return '--';
    }
    return new Intl.NumberFormat().format(value);
}

export function SettingInfo() {
    const t = useTranslations('setting');
    const latestInfoQuery = useLatestInfo();
    const nowVersionQuery = useNowVersion();
    const updateCore = useUpdateCore();
    const sqliteStatusQuery = useSQLiteStatus();
    const sqliteCheckpoint = useSQLiteCheckpoint();

    const backendNowVersion = nowVersionQuery.data || '';
    const latestVersion = latestInfoQuery.data?.tag_name || '';
    const sqliteStatus = sqliteStatusQuery.data;

    // 前端版本与后端当前版本不一致 → 浏览器缓存问题
    const isCacheMismatch = !!backendNowVersion && backendNowVersion !== APP_VERSION;
    // 最新版本与后端当前版本不一致 → 有新版本可更新
    const hasNewVersion = latestVersion && backendNowVersion && latestVersion !== backendNowVersion;

    const clearCacheAndReload = async () => {
        // 通知 Service Worker 清理缓存
        if ('serviceWorker' in navigator && navigator.serviceWorker.controller) {
            navigator.serviceWorker.controller.postMessage({ type: SW_MESSAGE_TYPE.CLEAR_CACHE });
        }
        // 同时也从主线程清理（双保险），但保留字体缓存
        if ('caches' in window) {
            const names = await caches.keys();
            await Promise.all(
                names
                    .filter((name) => isOctopusCacheName(name) && !isFontCacheName(name))
                    .map((name) => caches.delete(name))
            );
        }
        // 注销当前 SW，下次加载会重新注册
        if ('serviceWorker' in navigator) {
            const registrations = await navigator.serviceWorker.getRegistrations();
            await Promise.all(registrations.map((reg) => reg.unregister()));
        }
        // 强制刷新（跳过缓存）
        window.location.reload();
    };

    const handleForceRefresh = () => {
        clearCacheAndReload();
    };

    const handleUpdate = () => {
        updateCore.mutate(undefined, {
            onSuccess: () => {
                toast.success(t('info.updateSuccess'));
                // 更新成功后清理缓存并刷新
                setTimeout(() => {
                    clearCacheAndReload();
                }, 1500);
            },
            onError: () => {
                toast.error(t('info.updateFailed'));
            }
        });
    };

    const handleSQLiteCheckpoint = () => {
        sqliteCheckpoint.mutate(undefined, {
            onSuccess: (result) => {
                if (!result.is_sqlite) {
                    toast.warning(t('info.sqlite.notSqlite'));
                    return;
                }
                toast.success(t('info.sqlite.checkpointSuccess'), {
                    description: t('info.sqlite.checkpointSuccessDetail', {
                        walSize: formatBytes(result.wal_size_bytes_after),
                        busyFrames: formatCount(result.busy_frames),
                        checkpointedFrames: formatCount(result.checkpointed_frames),
                    }),
                });
                sqliteStatusQuery.refetch();
            },
            onError: () => {
                toast.error(t('info.sqlite.checkpointFailed'));
            },
        });
    };

    const sqliteRows = sqliteStatus?.is_sqlite ? [
        { label: t('info.sqlite.journalMode'), value: sqliteStatus.journal_mode || t('info.unknown') },
        { label: t('info.sqlite.autoVacuumMode'), value: sqliteStatus.auto_vacuum_mode || t('info.unknown') },
        { label: t('info.sqlite.walAutoCheckpoint'), value: formatCount(sqliteStatus.wal_auto_checkpoint) },
        { label: t('info.sqlite.walSize'), value: formatBytes(sqliteStatus.wal_size_bytes) },
        { label: t('info.sqlite.pageCount'), value: formatCount(sqliteStatus.page_count) },
        { label: t('info.sqlite.freelistCount'), value: formatCount(sqliteStatus.freelist_count) },
        { label: t('info.sqlite.dbPath'), value: sqliteStatus.db_path || '--', mono: true },
    ] : [];

    return (
        <div className="rounded-3xl border border-border bg-card p-6 space-y-5">
            <h2 className="text-lg font-bold text-card-foreground flex items-center gap-2">
                <Info className="h-5 w-5" />
                {t('info.title')}
            </h2>
            {/* GitHub 仓库 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Github className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('info.github')}</span>
                </div>
                <a
                    href={GITHUB_REPO}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-sm text-primary hover:underline"
                >
                    {GITHUB_REPO.replace('https://github.com/', '')}
                </a>
            </div>
            {/* 当前版本 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Tag className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('info.currentVersion')}</span>
                </div>
                <div className="flex items-center gap-2">
                    {nowVersionQuery.isLoading ? (
                        <Loader2 className="size-4 animate-spin text-muted-foreground" />
                    ) : (
                        <code className="text-sm font-mono text-muted-foreground">
                            {backendNowVersion || t('info.unknown')}
                        </code>
                    )}
                </div>
            </div>

            {/* 最新版本 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Download className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('info.latestVersion')}</span>
                </div>
                <div className="flex items-center gap-2">
                    {latestInfoQuery.isLoading ? (
                        <Loader2 className="size-4 animate-spin text-muted-foreground" />
                    ) : (
                        <code className="text-sm font-mono text-muted-foreground">
                            {latestVersion || t('info.unknown')}
                        </code>
                    )}
                </div>
            </div>

            <div className="rounded-2xl border border-border/70 bg-background/70 p-4 space-y-3">
                <div className="flex items-center justify-between gap-3">
                    <div className="flex items-center gap-3">
                        <Database className="h-5 w-5 text-muted-foreground" />
                        <div>
                            <p className="text-sm font-medium text-card-foreground">{t('info.sqlite.title')}</p>
                            <p className="text-xs text-muted-foreground">{t('info.sqlite.description')}</p>
                        </div>
                    </div>
                    <div className="flex items-center gap-2">
                        <Button
                            variant="outline"
                            size="sm"
                            onClick={handleSQLiteCheckpoint}
                            disabled={sqliteCheckpoint.isPending || sqliteStatusQuery.isLoading || !sqliteStatus?.is_sqlite}
                            className="rounded-xl"
                        >
                            <Database className={sqliteCheckpoint.isPending ? 'size-4 animate-pulse' : 'size-4'} />
                            {sqliteCheckpoint.isPending ? t('info.sqlite.checkpointRunning') : t('info.sqlite.checkpoint')}
                        </Button>
                        <Button
                            variant="outline"
                            size="sm"
                            onClick={() => sqliteStatusQuery.refetch()}
                            disabled={sqliteStatusQuery.isFetching}
                            className="rounded-xl"
                        >
                            <RefreshCw className={sqliteStatusQuery.isFetching ? 'size-4 animate-spin' : 'size-4'} />
                            {t('info.sqlite.refresh')}
                        </Button>
                    </div>
                </div>

                {sqliteStatusQuery.isLoading ? (
                    <div className="flex items-center gap-2 text-sm text-muted-foreground">
                        <Loader2 className="size-4 animate-spin" />
                        {t('info.sqlite.loading')}
                    </div>
                ) : sqliteStatusQuery.isError ? (
                    <div className="rounded-xl border border-destructive/20 bg-destructive/10 px-3 py-2 text-sm text-destructive">
                        {t('info.sqlite.loadFailed')}
                    </div>
                ) : !sqliteStatus?.is_sqlite ? (
                    <div className="rounded-xl border border-border/70 bg-card px-3 py-2 text-sm text-muted-foreground">
                        {t('info.sqlite.notSqlite')}
                    </div>
                ) : (
                    <>
                        {sqliteStatus.auto_vacuum_needs_vacuum && (
                            <div className="rounded-xl border border-amber-500/30 bg-amber-500/10 p-3 space-y-1">
                                <div className="flex items-start gap-3">
                                    <AlertTriangle className="mt-0.5 h-5 w-5 shrink-0 text-amber-600" />
                                    <div className="space-y-1">
                                        <p className="text-sm font-medium text-amber-700 dark:text-amber-300">
                                            {t('info.sqlite.repairRequired')}
                                        </p>
                                        <p className="text-xs text-muted-foreground">
                                            {t('info.sqlite.repairHint')}
                                        </p>
                                    </div>
                                </div>
                            </div>
                        )}

                        <div className="grid gap-3 md:grid-cols-2">
                            {sqliteRows.map((row) => (
                                <div key={row.label} className="rounded-2xl border border-border/60 bg-card px-3 py-3">
                                    <p className="text-xs text-muted-foreground">{row.label}</p>
                                    <p className={row.mono ? 'mt-1 break-all font-mono text-sm text-card-foreground' : 'mt-1 text-sm font-medium text-card-foreground'}>
                                        {row.value}
                                    </p>
                                </div>
                            ))}
                        </div>
                    </>
                )}
            </div>

            {/* 浏览器缓存问题警告 */}
            {isCacheMismatch && (
                <div className="p-3 bg-destructive/10 border border-destructive/20 rounded-xl space-y-2">
                    <div className="flex items-start gap-3">
                        <AlertTriangle className="h-5 w-5 text-destructive shrink-0 mt-0.5" />
                        <div className="flex-1 space-y-1">
                            <p className="text-sm text-destructive font-medium">
                                {t('info.versionMismatch')}
                            </p>
                            <p className="text-xs text-muted-foreground">
                                {t('info.versionMismatchHint', { frontend: APP_VERSION, backend: backendNowVersion })}
                            </p>
                        </div>
                    </div>
                    <div className="flex justify-end">
                        <Button
                            variant="destructive"
                            size="sm"
                            onClick={handleForceRefresh}
                            className="rounded-xl"
                        >
                            {t('info.forceRefresh')}
                        </Button>
                    </div>
                </div>
            )}

            {/* 有新版本可更新 */}
            {hasNewVersion && (
                <div className="p-3 bg-primary/10 border border-primary/20 rounded-xl space-y-2">
                    <div className="flex items-start gap-3">
                        <Download className="h-5 w-5 text-primary shrink-0 mt-0.5" />
                        <div className="flex-1 space-y-1">
                            <p className="text-sm text-primary font-medium">
                                {t('info.newVersionAvailable')}
                            </p>
                            <p className="text-xs text-muted-foreground">
                                {t('info.newVersionAvailableHint')}
                            </p>
                        </div>
                    </div>
                    <div className="flex justify-end">
                        <Button
                            variant="default"
                            size="sm"
                            onClick={handleUpdate}
                            disabled={updateCore.isPending}
                            className="rounded-xl"
                        >
                            {updateCore.isPending ? t('info.updating') : t('info.updateNow')}
                        </Button>
                    </div>
                </div>
            )}
        </div>
    );
}
