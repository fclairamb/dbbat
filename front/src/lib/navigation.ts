import {
  Activity,
  Database,
  FileText,
  Key,
  LayoutDashboard,
  Palette,
  Search,
  Server,
  Settings,
  Shield,
  ShieldCheck,
  ShieldQuestion,
  User,
  Users,
  UsersRound,
  type LucideIcon,
} from "lucide-react";

/**
 * One top-level section of the app. The sidebar entry, the page header icon
 * and the top-bar breadcrumb all read from here, so a section has one name and
 * one icon everywhere.
 */
export interface NavItem {
  title: string;
  icon: LucideIcon;
  href: string;
}

export const mainNavItems: NavItem[] = [
  { title: "Dashboard", icon: LayoutDashboard, href: "/" },
  { title: "Users", icon: Users, href: "/users" },
  { title: "User Groups", icon: UsersRound, href: "/user-groups" },
  { title: "Servers", icon: Database, href: "/servers" },
  { title: "Server Groups", icon: Server, href: "/server-groups" },
  { title: "Grants", icon: Shield, href: "/grants" },
  { title: "Grant Definitions", icon: ShieldCheck, href: "/grant-definitions" },
  { title: "Grant Requests", icon: ShieldQuestion, href: "/grant-requests" },
];

export const observabilityNavItems: NavItem[] = [
  { title: "Connections", icon: Activity, href: "/connections" },
  { title: "Queries", icon: Search, href: "/queries" },
  { title: "Audit Log", icon: FileText, href: "/audit" },
];

export const settingsNavItems: NavItem[] = [
  { title: "Settings", icon: Settings, href: "/settings" },
  { title: "API Keys", icon: Key, href: "/api-keys" },
];

/** Reachable by URL for everyone; listed in the sidebar in dev and test mode. */
export const designReferenceNavItem: NavItem = {
  title: "Design Reference",
  icon: Palette,
  href: "/design-reference",
};

/** Pages reached from the user menu rather than the sidebar. */
const otherNavItems: NavItem[] = [
  { title: "Profile", icon: User, href: "/profile" },
  { title: "Device Login", icon: Key, href: "/device" },
];

const allNavItems = [
  ...mainNavItems,
  ...observabilityNavItems,
  ...settingsNavItems,
  designReferenceNavItem,
  ...otherNavItems,
];

/** The section `href` names exactly (e.g. "/audit"), if any. */
export function navItem(href: string): NavItem | undefined {
  return allNavItems.find((item) => item.href === href);
}

/** The section a pathname lives in: "/users/abc" is in "/users". */
export function isInSection(pathname: string, href: string): boolean {
  if (href === "/") return pathname === "/";
  return pathname === href || pathname.startsWith(`${href}/`);
}
