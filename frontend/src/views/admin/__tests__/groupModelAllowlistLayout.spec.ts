import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

import { defineComponent, nextTick } from "vue";
import { flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { AdminGroup } from "@/types";
import GroupsView from "../GroupsView.vue";

const {
  listGroups,
  getModelAllowlistCandidates,
  getUsageSummary,
  getCapacitySummary,
  getLiveCapability,
  showSuccess,
  showError,
} = vi.hoisted(() => ({
  listGroups: vi.fn(),
  getModelAllowlistCandidates: vi.fn(),
  getUsageSummary: vi.fn(),
  getCapacitySummary: vi.fn(),
  getLiveCapability: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn(),
}));

const authState = vi.hoisted(() => ({ isSimpleMode: false }));

vi.mock("@/api/admin", () => ({
  adminAPI: {
    groups: {
      list: listGroups,
      getModelAllowlistCandidates,
      getUsageSummary,
      getCapacitySummary,
      getLiveCapability,
      getAll: vi.fn(),
      create: vi.fn(),
      update: vi.fn(),
      delete: vi.fn(),
      updateSortOrder: vi.fn(),
    },
    accounts: {
      list: vi.fn(),
      getById: vi.fn(),
    },
  },
}));

vi.mock("@/stores/app", () => ({
  useAppStore: () => ({ showSuccess, showError }),
}));

vi.mock("@/stores/auth", () => ({ useAuthStore: () => authState }));

vi.mock("@/stores/onboarding", () => ({
  useOnboardingStore: () => ({
    isCurrentStep: vi.fn(() => false),
    nextStep: vi.fn(),
  }),
}));

vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  };
});

const manualGroup = {
  id: 42,
  name: "Manual group",
  description: null,
  platform: "openai",
  rate_multiplier: 1,
  rpm_limit: 0,
  is_exclusive: false,
  status: "active",
  subscription_type: "standard",
  model_pricing: [],
  model_routing: null,
  model_allowlist: {
    enabled: false,
    models: ["legacy-manual-model"],
    source_account_count: 0,
  },
} as AdminGroup;

const AppLayoutStub = defineComponent({ template: "<main><slot /></main>" });
const TablePageLayoutStub = defineComponent({
  template: "<section><slot name='filters' /><slot name='table' /><slot name='pagination' /></section>",
});
const DataTableStub = defineComponent({
  props: {
    data: { type: Array, default: () => [] },
    columns: { type: Array, default: () => [] },
    loading: { type: Boolean, default: false },
  },
  template: "<div><div v-for='row in data' :key='row.id'><slot name='cell-actions' :row='row' /></div></div>",
});
const BaseDialogStub = defineComponent({
  props: { show: { type: Boolean, default: false } },
  template: "<div v-if='show'><slot /><slot name='footer' /></div>",
});

const mountGroupsView = () =>
  mount(GroupsView, {
    global: {
      stubs: {
        AppLayout: AppLayoutStub,
        TablePageLayout: TablePageLayoutStub,
        DataTable: DataTableStub,
        Pagination: true,
        BaseDialog: BaseDialogStub,
        ConfirmDialog: true,
        EmptyState: true,
        Select: true,
        PlatformIcon: true,
        Icon: true,
        GroupCapacityBadge: true,
        GroupRateMultipliersModal: true,
        GroupRPMOverridesModal: true,
        VueDraggable: true,
      },
    },
  });

const currentDir = dirname(fileURLToPath(import.meta.url));
const groupsViewSource = readFileSync(
  resolve(currentDir, "../GroupsView.vue"),
  "utf8",
);

describe("groups model allowlist layout", () => {
  it("keeps the toolbar outside of the scrolling list content", () => {
    expect(groupsViewSource).toContain("overflow-hidden rounded-lg border");
    expect(groupsViewSource).toContain("max-h-64 space-y-2 overflow-y-auto p-2");
    expect(groupsViewSource).not.toContain("sticky top-0");
  });

  it("uses a wide dialog and keeps model pricing controls responsive", () => {
    expect(groupsViewSource).toContain('width="wide"');
    expect(groupsViewSource).toContain(
      "btn btn-secondary shrink-0 whitespace-nowrap",
    );
  });

  it("renders automatic allowlists as read-only summaries and manual controls only when disabled", () => {
    expect(groupsViewSource.match(/data-testid="group-model-allowlist-auto"/g)).toHaveLength(2);
    expect(groupsViewSource.match(/data-testid="group-model-allowlist-manual"/g)).toHaveLength(2);
    expect(groupsViewSource).toContain("admin.groups.modelAllowlist.autoSourceCount");
    expect(groupsViewSource).toContain("admin.groups.modelAllowlist.autoEmpty");
    expect(groupsViewSource).toContain("v-for=\"model in editModelAllowlistSummary.models\"");
    expect(groupsViewSource).toContain("v-model=\"item.selected\"");
    expect(groupsViewSource).not.toContain("modelAllowlistSelectedCount.value === 0");
  });
});

describe("GroupsView automatic allowlist transition", () => {
  beforeEach(() => {
    authState.isSimpleMode = false;
    localStorage.clear();
    for (const fn of [
      listGroups,
      getModelAllowlistCandidates,
      getUsageSummary,
      getCapacitySummary,
      getLiveCapability,
      showSuccess,
      showError,
    ]) {
      fn.mockReset();
    }
    listGroups.mockResolvedValue({
      items: [manualGroup],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1,
    });
    getModelAllowlistCandidates.mockResolvedValue([]);
    getUsageSummary.mockResolvedValue([]);
    getCapacitySummary.mockResolvedValue([]);
    getLiveCapability.mockResolvedValue({ supported: false });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("clears disabled manual values immediately when switched to automatic mode", async () => {
    const wrapper = mountGroupsView();
    try {
      await flushPromises();
      const editButton = wrapper
        .findAll("button")
        .find((button) => button.text().trim() === "common.edit");
      expect(editButton).toBeTruthy();
      await editButton!.trigger("click");
      await flushPromises();

      expect(wrapper.get('[data-testid="group-model-allowlist-manual"]').text())
        .toContain("legacy-manual-model");
      await wrapper
        .get('[data-testid="group-model-allowlist-toggle"]')
        .trigger("click");
      await nextTick();

      const automaticSummary = wrapper.get(
        '[data-testid="group-model-allowlist-auto"]',
      );
      expect(automaticSummary.text()).toContain(
        "admin.groups.modelAllowlist.autoEmpty",
      );
      expect(automaticSummary.text()).not.toContain("legacy-manual-model");
      expect(automaticSummary.find("li").exists()).toBe(false);
    } finally {
      wrapper.unmount();
    }
  });
});
