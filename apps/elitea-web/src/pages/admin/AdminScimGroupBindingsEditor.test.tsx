/**
 * Rendering + write-path guard for the SCIM group-binding editor.
 *
 * What is asserted here is what a status-code test cannot see:
 *
 *  1. **A new binding POSTs and an edited one PUTs its id.** The two verbs mean
 *     "author a binding" and "re-author that binding"; sending the wrong one
 *     either creates a duplicate group name (refused) or edits nothing.
 *  2. **The screen distinguishes a membership the group GRANTED from one it
 *     merely found.** They behave differently on removal, and an operator who
 *     could not tell them apart would expect a group removal to take somebody
 *     out of a project it will not touch.
 *  3. **A refusal renders the SERVER's own sentence**, which names the project
 *     or the role that was wrong — and the dialog STAYS OPEN holding it.
 *  4. **The delete confirmation says what it withdraws**, because a group
 *     binding removal takes access away from everybody the group granted.
 */
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpResponse, http } from "msw";

import {
  configureGeneratedClient,
  resetGeneratedClient,
} from "@/shared/api/generated/mutator";
import { server } from "@/test/setup";

import { AdminScimGroupBindingsEditor } from "./AdminScimGroupBindingsEditor";
import { confirmDangerousAction } from "./__tests__/dangerousDialog";
import { renderAdminRoute } from "./__tests__/testRouter";

const BINDING_TEMPLATE = {
  id: "7",
  display_name: "Platform Team",
  external_id: "grp-1",
  project_id: 12,
  project_name: "Platform",
  role_name: "editor",
  members: [
    { user_id: 1, user_name: "e2e-admin@autotest.local", granted: true },
    { user_id: 2, user_name: "e2e-member@autotest.local", granted: false },
  ],
};

const BINDINGS = [
  {
    id: "7",
    display_name: "Platform Team",
    external_id: "grp-1",
    project_id: 12,
    project_name: "Platform",
    role_name: "editor",
    members: [
      { user_id: 1, user_name: "e2e-admin@autotest.local", granted: true },
      { user_id: 2, user_name: "e2e-member@autotest.local", granted: false },
    ],
  },
];

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

function useBindingHandlers(
  options: {
    saveStatus?: number;
    saveBody?: Record<string, string>;
    bindings?: (typeof BINDINGS)[number][];
    total?: number;
    projectRoles?: string[];
  } = {},
): void {
  server.use(
    http.get(
      "*/admin/scim_group_bindings/administration/project_roles/*",
      ({ request }) => {
        recorded.push({ method: "GET", url: request.url, body: null });
        return HttpResponse.json({
          roles: options.projectRoles ?? [
            "admin",
            "editor",
            "viewer",
            "system",
          ],
          total: 4,
        });
      },
    ),
    http.get("*/admin/scim_group_bindings/administration", ({ request }) => {
      recorded.push({ method: "GET", url: request.url, body: null });
      const url = new URL(request.url);
      const offset = Number(url.searchParams.get("offset") ?? "0");
      const limit = Number(url.searchParams.get("limit") ?? "100");
      const all = options.bindings ?? BINDINGS;
      return HttpResponse.json({
        bindings: all.slice(offset, offset + limit),
        total: options.total ?? all.length,
        limit,
        offset,
      });
    }),
    http.post(
      "*/admin/scim_group_bindings/administration",
      async ({ request }) => {
        recorded.push({
          method: "POST",
          url: request.url,
          body: await request.json(),
        });
        if (options.saveStatus !== undefined) {
          return HttpResponse.json(options.saveBody, {
            status: options.saveStatus,
          });
        }
        return HttpResponse.json({ binding: BINDINGS[0] }, { status: 201 });
      },
    ),
    http.put(
      "*/admin/scim_group_bindings/administration/*",
      async ({ request }) => {
        recorded.push({
          method: "PUT",
          url: request.url,
          body: await request.json(),
        });
        if (options.saveStatus !== undefined) {
          return HttpResponse.json(options.saveBody, {
            status: options.saveStatus,
          });
        }
        return HttpResponse.json({ binding: BINDINGS[0] });
      },
    ),
    http.delete(
      "*/admin/scim_group_bindings/administration/*",
      ({ request }) => {
        recorded.push({
          method: "DELETE",
          url: request.url,
          body: { reasonCode: request.headers.get("Elitea-Reason-Code") },
        });
        return new HttpResponse(null, { status: 204 });
      },
    ),
    // The project picker's sources. They are separate permissions from the
    // binding surface, which is why the dialog has a fallback for their absence.
    http.get("*/admin/projects/administration", () =>
      HttpResponse.json({ rows: [{ id: 12, name: "Platform" }], total: 1 }),
    ),
    // The GENERAL role listing, which answers a hardcoded admin/editor/viewer
    // for a project with no roles. It is stubbed with a set this screen must
    // never render, so a test fails if the dialog reads it.
    http.get("*/admin/roles/administration/*", () =>
      HttpResponse.json({
        rows: [{ id: "9", name: "from-the-general-listing" }],
        total: 1,
      }),
    ),
  );
}

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method !== "GET");
}

beforeEach(() => {
  recorded = [];
  configureGeneratedClient({ baseUrl: "/api/v2" });
  useBindingHandlers();
});

afterEach(() => {
  resetGeneratedClient();
});

describe("Admin › Authentication › SCIM group bindings", () => {
  it("lists a binding with the project and the role it grants", async () => {
    renderAdminRoute(<AdminScimGroupBindingsEditor />);

    expect(await screen.findByText("Platform Team")).toBeInTheDocument();
    expect(screen.getByText("Platform (#12)")).toBeInTheDocument();
    expect(screen.getByText("editor")).toBeInTheDocument();
  });

  it("says who the group granted and who it merely found", async () => {
    renderAdminRoute(<AdminScimGroupBindingsEditor />);
    await screen.findByText("Platform Team");

    expect(screen.getByText("1 granted")).toBeInTheDocument();
    // The member the group did not create keeps their access when the group
    // lets go. The screen must not present them as the same thing.
    expect(screen.getByText("1 already members")).toBeInTheDocument();
  });

  it("reads the bindings from the administration mode", async () => {
    renderAdminRoute(<AdminScimGroupBindingsEditor />);
    await screen.findByText("Platform Team");

    expect(recorded[0]?.url).toContain(
      "/admin/scim_group_bindings/administration",
    );
  });

  it("POSTs a new binding and PUTs an edited one", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimGroupBindingsEditor />);
    await screen.findByText("Platform Team");

    await user.click(screen.getByTestId("admin-scim-group-bindings-add"));
    await user.type(screen.getByTestId("admin-scim-binding-name"), "Finance");
    await user.click(screen.getByLabelText("Project"));
    await user.click(
      await screen.findByText("Platform (#12)", { selector: "li" }),
    );
    await user.click(screen.getByLabelText("Role"));
    await user.click(await screen.findByRole("option", { name: "viewer" }));
    await user.click(screen.getByTestId("admin-scim-binding-save"));
    await confirmDangerousAction(user, "Finance");

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    const created = writes()[0];
    expect(created?.method).toBe("POST");
    expect(created?.body).toEqual({
      display_name: "Finance",
      project_id: 12,
      role_name: "viewer",
      reason_code: "incident",
      reason: "ticket 42",
    });

    // And the same screen, editing the existing row, addresses it by id.
    const row = screen.getByText("Platform Team").closest("tr");
    const edit = Array.from(row?.querySelectorAll("button") ?? []).find(
      (button) => button.textContent === "Edit",
    );
    await user.click(edit as HTMLElement);
    await user.click(screen.getByTestId("admin-scim-binding-save"));
    await confirmDangerousAction(user, "Platform Team", { code: "correction" });

    await waitFor(() => {
      expect(writes()).toHaveLength(2);
    });
    expect(writes()[1]?.body).toMatchObject({ reason_code: "correction" });
    expect(writes()[1]?.method).toBe("PUT");
    expect(writes()[1]?.url).toContain(
      "/admin/scim_group_bindings/administration/7",
    );
  });

  it("keeps the dialog open on a refusal and renders the server sentence", async () => {
    useBindingHandlers({
      saveStatus: 400,
      saveBody: { error: 'project 12 has no role "auditor"' },
    });
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimGroupBindingsEditor />);
    await screen.findByText("Platform Team");

    const row = screen.getByText("Platform Team").closest("tr");
    const edit = Array.from(row?.querySelectorAll("button") ?? []).find(
      (button) => button.textContent === "Edit",
    );
    await user.click(edit as HTMLElement);
    await user.click(screen.getByTestId("admin-scim-binding-save"));
    await confirmDangerousAction(user, "Platform Team");

    expect(
      await screen.findByTestId("admin-scim-binding-dialog-error"),
    ).toHaveTextContent("auditor");
    // Still open, still holding what was typed.
    expect(screen.getByTestId("admin-scim-binding-save")).toBeInTheDocument();
  });

  it("confirms a removal by saying what it withdraws, then deletes by id", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimGroupBindingsEditor />);
    await screen.findByText("Platform Team");

    const row = screen.getByText("Platform Team").closest("tr");
    const remove = Array.from(row?.querySelectorAll("button") ?? []).find(
      (button) => button.textContent === "Remove",
    );
    await user.click(remove as HTMLElement);

    const dialog = await screen.findByTestId("admin-dangerous-dialog");
    expect(dialog).toHaveTextContent("withdraws");
    expect(dialog).toHaveTextContent("added by hand are not affected");

    await confirmDangerousAction(user, "Platform Team");
    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    expect(writes()[0]?.method).toBe("DELETE");
    expect(writes()[0]?.url).toContain(
      "/admin/scim_group_bindings/administration/7",
    );
    expect(writes()[0]?.body).toEqual({ reasonCode: "incident" });
  });

  it("offers the project's own roles, never the general listing's defaults", async () => {
    useBindingHandlers({
      projectRoles: ["admin", "editor", "viewer", "system"],
    });
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimGroupBindingsEditor />);
    await screen.findByText("Platform Team");

    await user.click(screen.getByTestId("admin-scim-group-bindings-add"));
    await user.click(screen.getByLabelText("Project"));
    await user.click(
      await screen.findByText("Platform (#12)", { selector: "li" }),
    );
    await user.click(screen.getByLabelText("Role"));

    // `system` is a real project role and is offered; the general listing's
    // fallback set does not carry it, and its marker value must be absent.
    expect(
      await screen.findByRole("option", { name: "system" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("option", { name: "from-the-general-listing" }),
    ).not.toBeInTheDocument();
  });

  it("says a project has no roles rather than offering defaults", async () => {
    useBindingHandlers({ projectRoles: [] });
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimGroupBindingsEditor />);
    await screen.findByText("Platform Team");

    await user.click(screen.getByTestId("admin-scim-group-bindings-add"));
    await user.click(screen.getByLabelText("Project"));
    await user.click(
      await screen.findByText("Platform (#12)", { selector: "li" }),
    );

    // A control that offered a role here would offer a value its own save
    // refuses.
    expect(
      await screen.findByTestId("admin-scim-binding-no-roles"),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Role")).not.toBeInTheDocument();
  });

  it("pages rather than hiding the bindings past the first page", async () => {
    const many = Array.from({ length: 120 }, (_, index) => ({
      ...BINDING_TEMPLATE,
      id: String(index + 1),
      display_name: `Group ${String(index + 1)}`,
    }));
    useBindingHandlers({ bindings: many });
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimGroupBindingsEditor />);

    await screen.findByText("Group 1");
    // The screen says what it is NOT showing, so a binding past the page is
    // known to exist rather than assumed absent.
    expect(
      screen.getByTestId("admin-scim-group-bindings-paging"),
    ).toHaveTextContent("Showing 1–100 of 120 bindings");
    expect(screen.queryByText("Group 101")).not.toBeInTheDocument();

    await user.click(screen.getByTestId("admin-scim-group-bindings-next"));

    // And the rest are reachable.
    expect(await screen.findByText("Group 101")).toBeInTheDocument();
    expect(screen.queryByText("Group 1")).not.toBeInTheDocument();
  });

  it('does not report "no group is bound" while the listing is failing', async () => {
    server.use(
      http.get("*/admin/scim_group_bindings/administration", () =>
        HttpResponse.json({ error: "nope" }, { status: 503 }),
      ),
    );
    renderAdminRoute(<AdminScimGroupBindingsEditor />);

    expect(
      await screen.findByTestId("admin-scim-group-bindings-error"),
    ).toBeInTheDocument();
    // "No group is bound" is a sentence an operator acts on. It must never be
    // the rendering of a failed read.
    expect(
      screen.queryByTestId("admin-scim-group-bindings-empty"),
    ).not.toBeInTheDocument();
  });
});
