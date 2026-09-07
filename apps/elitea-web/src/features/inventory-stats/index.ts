/** Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20). */
export {
  useInventoryStats,
  useMaintenance,
  type InventoryStatsView,
  type MaintenanceRun,
  type MaintenanceTool,
} from './model/useInventoryStats';
export { InventoryStatsPanel } from './ui/InventoryStatsPanel';
